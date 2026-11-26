package messaging

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/fastclaw-ai/weclaw/agent"
	"github.com/fastclaw-ai/weclaw/ilink"
)

const (
	defaultWorkspaceTimeout    = 10 * time.Minute
	defaultWorkspaceOutput     = 64 << 10
	defaultWorkspaceArchive    = 50 << 20
	defaultWorkspaceName       = "main"
	workspaceMaxListedEntries  = 200
	workspaceMaxUntrackedDiffs = 12
	workspaceMaxReadFile       = 512 << 10
)

// WorkspaceAgentFactory starts an agent whose working directory is cwd.
type WorkspaceAgentFactory func(ctx context.Context, name, cwd string) (agent.Agent, error)

// WorkspaceOptions configures the isolated code workspace.
type WorkspaceOptions struct {
	Enabled         bool
	Root            string
	Agent           string
	AllowShell      bool
	Timeout         time.Duration
	MaxOutputBytes  int
	MaxArchiveBytes int64
}

type workspaceUser struct {
	mu        sync.Mutex
	root      string
	current   string
	agent     agent.Agent
	agentName string
}

// WorkspaceManager keeps per-user workspaces under a single root.
type WorkspaceManager struct {
	opts    WorkspaceOptions
	factory WorkspaceAgentFactory
	mu      sync.Mutex
	users   map[string]*workspaceUser
}

// NewWorkspaceManager prepares the workspace root. A nil manager means the
// feature is disabled.
func NewWorkspaceManager(opts WorkspaceOptions, factory WorkspaceAgentFactory) (*WorkspaceManager, error) {
	if !opts.Enabled {
		return nil, nil
	}
	if opts.Root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		opts.Root = filepath.Join(home, ".weclaw", "workspaces")
	}
	root, err := filepath.Abs(opts.Root)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("create workspace root: %w", err)
	}
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, fmt.Errorf("resolve workspace root: %w", err)
	}
	opts.Root = realRoot
	if opts.Timeout <= 0 {
		opts.Timeout = defaultWorkspaceTimeout
	}
	if opts.Timeout < 10*time.Second {
		opts.Timeout = 10 * time.Second
	}
	if opts.Timeout > time.Hour {
		opts.Timeout = time.Hour
	}
	if opts.MaxOutputBytes <= 0 {
		opts.MaxOutputBytes = defaultWorkspaceOutput
	}
	if opts.MaxOutputBytes < 4<<10 {
		opts.MaxOutputBytes = 4 << 10
	}
	if opts.MaxOutputBytes > 1<<20 {
		opts.MaxOutputBytes = 1 << 20
	}
	if opts.MaxArchiveBytes <= 0 {
		opts.MaxArchiveBytes = defaultWorkspaceArchive
	}
	if opts.MaxArchiveBytes < 1<<20 {
		opts.MaxArchiveBytes = 1 << 20
	}
	if opts.MaxArchiveBytes > 500<<20 {
		opts.MaxArchiveBytes = 500 << 20
	}
	m := &WorkspaceManager{opts: opts, factory: factory, users: make(map[string]*workspaceUser)}
	log.Printf("[workspace] enabled root=%s agent=%q shell=%t timeout=%s output=%dKB archive=%dMB",
		opts.Root, opts.Agent, opts.AllowShell, opts.Timeout, opts.MaxOutputBytes>>10, opts.MaxArchiveBytes>>20)
	return m, nil
}

// Close stops any agent processes started for workspaces.
func (m *WorkspaceManager) Close() {
	if m == nil {
		return
	}
	m.mu.Lock()
	users := make([]*workspaceUser, 0, len(m.users))
	for _, u := range m.users {
		users = append(users, u)
	}
	m.mu.Unlock()
	for _, u := range users {
		u.mu.Lock()
		stopAgent(u.agent)
		u.agent = nil
		u.mu.Unlock()
	}
}

func stopAgent(ag agent.Agent) {
	if stopper, ok := ag.(interface{ Stop() }); ok {
		stopper.Stop()
	}
}

// CurrentRoot returns the active workspace directory for a sender.
func (m *WorkspaceManager) CurrentRoot(userID string) string {
	if m == nil {
		return ""
	}
	root, _, err := m.currentRoot(context.Background(), userID, false)
	if err != nil {
		return ""
	}
	return root
}

func (m *WorkspaceManager) getUser(userID string) (*workspaceUser, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if u, ok := m.users[userID]; ok {
		return u, nil
	}
	root := filepath.Join(m.opts.Root, workspaceUserDirName(userID))
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("create user workspace dir: %w", err)
	}
	u := &workspaceUser{root: root, current: defaultWorkspaceName}
	m.users[userID] = u
	return u, nil
}

func workspaceUserDirName(userID string) string {
	var b strings.Builder
	for _, r := range userID {
		if b.Len() >= 24 {
			break
		}
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	name := strings.Trim(b.String(), "-")
	if name == "" {
		name = "user"
	}
	sum := sha1.Sum([]byte(userID))
	return name + "-" + hex.EncodeToString(sum[:4])
}

func validWorkspaceName(name string) bool {
	if name == "" || name == "." || name == ".." || len(name) > 48 {
		return false
	}
	for _, r := range name {
		if r == '/' || r == '\\' || unicode.IsControl(r) {
			return false
		}
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_' || r == '.' {
			continue
		}
		return false
	}
	return true
}

func (m *WorkspaceManager) currentRoot(ctx context.Context, userID string, create bool) (string, string, error) {
	u, err := m.getUser(userID)
	if err != nil {
		return "", "", err
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.current == "" {
		u.current = defaultWorkspaceName
	}
	name := u.current
	dir := filepath.Join(u.root, name)
	info, err := os.Lstat(dir)
	if os.IsNotExist(err) {
		if !create {
			return "", name, fmt.Errorf("workspace %q does not exist", name)
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return "", name, err
		}
		initWorkspaceGit(dir)
	} else if err != nil {
		return "", name, err
	} else if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return "", name, fmt.Errorf("workspace %q is not a regular directory", name)
	}
	return dir, name, nil
}

func initWorkspaceGit(dir string) {
	git, err := exec.LookPath("git")
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, git, "init", "-q")
	cmd.Dir = dir
	cmd.Env = gitEnv()
	if out, err := cmd.CombinedOutput(); err != nil {
		log.Printf("[workspace] git init %s failed: %v (%s)", dir, err, strings.TrimSpace(string(out)))
	}
}

func gitEnv() []string {
	return append(os.Environ(), "GIT_PAGER=cat", "GIT_TERMINAL_PROMPT=0", "PAGER=cat", "NO_COLOR=1")
}

func (m *WorkspaceManager) workspaceList(userID string) string {
	u, err := m.getUser(userID)
	if err != nil {
		return "工作区打不开：" + err.Error()
	}
	u.mu.Lock()
	current := u.current
	if current == "" {
		current = defaultWorkspaceName
	}
	userRoot := u.root
	u.mu.Unlock()

	entries, err := os.ReadDir(userRoot)
	if err != nil {
		return "工作区读取失败：" + err.Error()
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)
	var lines []string
	lines = append(lines, "代码工作区："+current)
	if len(names) == 0 {
		lines = append(lines, "还没有工作区。发送 /code <任务> 会自动创建 main。")
	} else {
		lines = append(lines, "工作区列表：")
		for _, name := range names {
			mark := "  "
			if name == current {
				mark = "* "
			}
			lines = append(lines, mark+name)
		}
	}
	lines = append(lines, "切换：/ws use <名称>", "新建：/ws new <名称>", "导入：/ws import <公开 GitHub 链接>，或先发 zip 再 /ws import")
	lines = append(lines, "文件：/files /cat /write，差异：/diff，打包：/pack，回传：/send")
	if m.opts.AllowShell {
		lines = append(lines, "命令执行：已开启（/run）")
		lines = append(lines, "注意：/run 以机器人进程权限执行，不是安全沙箱。")
	} else {
		lines = append(lines, "命令执行：关闭（配置 workspace.allow_shell=true 后可用 /run）")
	}
	return strings.Join(lines, "\n")
}

func (m *WorkspaceManager) switchCommand(ctx context.Context, userID, arg string) string {
	fields := strings.Fields(arg)
	if len(fields) == 0 || fields[0] == "list" {
		return m.workspaceList(userID)
	}
	u, err := m.getUser(userID)
	if err != nil {
		return "工作区打不开：" + err.Error()
	}
	switch fields[0] {
	case "new":
		name := ""
		if len(fields) > 1 {
			name = fields[1]
		}
		if name == "" {
			name = "ws-" + time.Now().Format("20060102-150405")
		}
		if !validWorkspaceName(name) {
			return "工作区名只能用字母、数字、点、下划线和短横线，最长 48 个字符。"
		}
		dir := filepath.Join(u.root, name)
		if _, err := os.Lstat(dir); err == nil {
			return "工作区已存在：" + name
		} else if !os.IsNotExist(err) {
			return "检查工作区失败：" + err.Error()
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return "创建工作区失败：" + err.Error()
		}
		initWorkspaceGit(dir)
		u.mu.Lock()
		u.current = name
		if u.agent != nil {
			u.agent.SetCwd(dir)
		}
		u.mu.Unlock()
		return "已新建并切换到工作区：" + name
	case "use":
		if len(fields) < 2 {
			return "用法：/ws use <名称>"
		}
		name := fields[1]
		if !validWorkspaceName(name) {
			return "工作区名不合法。"
		}
		dir := filepath.Join(u.root, name)
		info, err := os.Lstat(dir)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return "没有这个工作区：" + name
		}
		u.mu.Lock()
		u.current = name
		if u.agent != nil {
			u.agent.SetCwd(dir)
		}
		u.mu.Unlock()
		return "已切换到工作区：" + name
	default:
		return "用法：/ws [list] /ws new <名称> /ws use <名称> /ws import <公开 GitHub 链接>"
	}
}

func isWithinRoot(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}

func resolveWorkspacePath(root, raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("path is empty")
	}
	if strings.ContainsRune(raw, 0) || filepath.IsAbs(raw) {
		return "", fmt.Errorf("path must be relative")
	}
	clean := filepath.Clean(raw)
	if clean == "." {
		return root, nil
	}
	candidate := filepath.Join(root, clean)
	if !isWithinRoot(root, candidate) {
		return "", fmt.Errorf("path escapes workspace")
	}

	probe := candidate
	for {
		if _, err := os.Lstat(probe); err == nil {
			real, err := filepath.EvalSymlinks(probe)
			if err != nil {
				return "", err
			}
			if !isWithinRoot(root, real) {
				return "", fmt.Errorf("path escapes workspace through a symlink")
			}
			break
		} else if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			break
		}
		probe = parent
	}
	return candidate, nil
}

func (m *WorkspaceManager) listFiles(ctx context.Context, userID, sub string) string {
	root, name, err := m.currentRoot(ctx, userID, true)
	if err != nil {
		return "工作区不可用：" + err.Error()
	}
	target, err := resolveWorkspacePath(root, sub)
	if err != nil {
		return "路径不合法：" + err.Error()
	}
	info, err := os.Stat(target)
	if err != nil {
		return "目录不存在：" + err.Error()
	}
	if !info.IsDir() {
		return "不是目录。用 /cat 查看文件。"
	}
	entries, err := os.ReadDir(target)
	if err != nil {
		return "读取目录失败：" + err.Error()
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	var lines []string
	rel, _ := filepath.Rel(root, target)
	if rel == "." {
		rel = ""
	}
	lines = append(lines, "工作区 "+name+"，目录 /"+filepath.ToSlash(rel))
	shown := 0
	for _, entry := range entries {
		if entry.Name() == ".git" {
			continue
		}
		if shown >= workspaceMaxListedEntries {
			lines = append(lines, fmt.Sprintf("仅显示前 %d 项。", workspaceMaxListedEntries))
			break
		}
		shown++
		info, err := entry.Info()
		if err != nil {
			lines = append(lines, entry.Name()+"  (无法读取)")
			continue
		}
		switch {
		case entry.IsDir():
			lines = append(lines, entry.Name()+"/")
		case info.Mode()&os.ModeSymlink != 0:
			lines = append(lines, entry.Name()+"  -> 链接")
		default:
			lines = append(lines, fmt.Sprintf("%s  %s", entry.Name(), humanSize(info.Size())))
		}
	}
	if shown == 0 {
		lines = append(lines, "（空目录）")
	}
	return strings.Join(lines, "\n")
}

func humanSize(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

func (m *WorkspaceManager) readFile(ctx context.Context, userID, raw string) string {
	root, name, err := m.currentRoot(ctx, userID, true)
	if err != nil {
		return "工作区不可用：" + err.Error()
	}
	target, err := resolveWorkspacePath(root, raw)
	if err != nil {
		return "路径不合法：" + err.Error()
	}
	info, err := os.Stat(target)
	if err != nil {
		return "读取失败：" + err.Error()
	}
	if !info.Mode().IsRegular() {
		return "不是普通文件。"
	}
	if info.Size() > workspaceMaxReadFile {
		return fmt.Sprintf("文件太大（%s），请用 /send 直接取回。", humanSize(info.Size()))
	}
	data, err := os.ReadFile(target)
	if err != nil {
		return "读取失败：" + err.Error()
	}
	truncated := false
	if len(data) > m.opts.MaxOutputBytes {
		data = data[:m.opts.MaxOutputBytes]
		truncated = true
	}
	if bytes.IndexByte(data, 0) >= 0 {
		return "这是二进制文件，请用 /send 取回。"
	}
	rel, _ := filepath.Rel(root, target)
	header := fmt.Sprintf("工作区 %s：/%s", name, filepath.ToSlash(rel))
	if truncated {
		header += "\n（内容已截断）"
	}
	return header + "\n\n" + string(data)
}

func (m *WorkspaceManager) writeFile(ctx context.Context, userID, raw, content string) string {
	root, name, err := m.currentRoot(ctx, userID, true)
	if err != nil {
		return "工作区不可用：" + err.Error()
	}
	target, err := resolveWorkspacePath(root, raw)
	if err != nil {
		return "路径不合法：" + err.Error()
	}
	if target == root {
		return "请指定文件名。"
	}
	if len(content) > workspaceMaxReadFile {
		return "内容太大，单次写入最多 512 KB。"
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return "创建目录失败：" + err.Error()
	}
	parentReal, err := filepath.EvalSymlinks(filepath.Dir(target))
	if err != nil || !isWithinRoot(root, parentReal) {
		return "目录越过工作区边界，已拒绝。"
	}
	if info, err := os.Lstat(target); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return "目标是链接或非普通文件，已拒绝。"
		}
	}
	if err := os.WriteFile(target, []byte(content), 0o644); err != nil {
		return "写入失败：" + err.Error()
	}
	rel, _ := filepath.Rel(root, target)
	return fmt.Sprintf("已写入工作区 %s：/%s（%s）", name, filepath.ToSlash(rel), humanSize(int64(len(content))))
}

func (m *WorkspaceManager) runCommand(ctx context.Context, userID, command string) string {
	if !m.opts.AllowShell {
		return "/run 未启用。配置里把 workspace.allow_shell 设为 true 才能执行命令。"
	}
	command = strings.TrimSpace(command)
	if command == "" {
		return "用法：/run <命令>"
	}
	root, _, err := m.currentRoot(ctx, userID, true)
	if err != nil {
		return "工作区不可用：" + err.Error()
	}
	runCtx, cancel := context.WithTimeout(ctx, m.opts.Timeout)
	defer cancel()

	var name string
	var args []string
	if runtime.GOOS == "windows" {
		name, args = "cmd", []string{"/C", command}
	} else {
		name, args = "/bin/sh", []string{"-c", command}
	}
	cmd := exec.CommandContext(runCtx, name, args...)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "GIT_PAGER=cat", "PAGER=cat", "NO_COLOR=1", "TERM=dumb", "CLICOLOR=0")
	configureWorkspaceCommand(cmd)
	cmd.WaitDelay = 3 * time.Second
	var out cappedBuffer
	out.limit = m.opts.MaxOutputBytes
	cmd.Stdout = &out
	cmd.Stderr = &out
	runErr := cmd.Run()

	text := strings.TrimSpace(out.String())
	if text == "" {
		text = "（没有输出）"
	}
	if out.truncated {
		text += fmt.Sprintf("\n\n（输出已截断，最多 %d KB）", m.opts.MaxOutputBytes>>10)
	}
	if runCtx.Err() == context.DeadlineExceeded {
		return text + fmt.Sprintf("\n\n（命令超时，已终止，上限 %s）", m.opts.Timeout)
	}
	if runErr != nil {
		if exitErr, ok := runErr.(*exec.ExitError); ok {
			text += fmt.Sprintf("\n\n（退出码 %d）", exitErr.ExitCode())
		} else {
			text += "\n\n（执行失败：" + runErr.Error() + "）"
		}
	}
	return text
}

type cappedBuffer struct {
	mu        sync.Mutex
	buf       bytes.Buffer
	limit     int
	truncated bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	remaining := b.limit - b.buf.Len()
	if remaining > 0 {
		if len(p) <= remaining {
			b.buf.Write(p)
		} else {
			b.buf.Write(p[:remaining])
			b.truncated = true
		}
	} else if len(p) > 0 {
		b.truncated = true
	}
	return len(p), nil
}

func (b *cappedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func runGit(ctx context.Context, dir string, limit int, args ...string) (string, error) {
	git, err := exec.LookPath("git")
	if err != nil {
		return "", err
	}
	cmd := exec.CommandContext(ctx, git, args...)
	cmd.Dir = dir
	cmd.Env = gitEnv()
	var out cappedBuffer
	out.limit = limit
	cmd.Stdout = &out
	cmd.Stderr = &out
	err = cmd.Run()
	return strings.TrimSpace(out.String()), err
}

func (m *WorkspaceManager) diff(ctx context.Context, userID string) string {
	root, _, err := m.currentRoot(ctx, userID, true)
	if err != nil {
		return "工作区不可用：" + err.Error()
	}
	diffCtx, cancel := context.WithTimeout(ctx, m.opts.Timeout)
	defer cancel()
	if _, err := exec.LookPath("git"); err != nil {
		return "没有找到 git，/diff 用不了。"
	}
	inside, err := runGit(diffCtx, root, 4096, "rev-parse", "--is-inside-work-tree")
	if err != nil || strings.TrimSpace(inside) != "true" {
		return "当前工作区不是 git 仓库，/diff 用不了。"
	}

	status, _ := runGit(diffCtx, root, m.opts.MaxOutputBytes, "status", "--short")
	diff, _ := runGit(diffCtx, root, m.opts.MaxOutputBytes, "diff", "--no-color")
	cached, _ := runGit(diffCtx, root, m.opts.MaxOutputBytes, "diff", "--cached", "--no-color")

	var b strings.Builder
	if status != "" {
		b.WriteString("状态：\n")
		b.WriteString(status)
	}
	if diff != "" {
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString("未暂存改动：\n")
		b.WriteString(diff)
	}
	if cached != "" {
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString("已暂存改动：\n")
		b.WriteString(cached)
	}

	untracked, _ := runGit(diffCtx, root, 1<<20, "ls-files", "--others", "--exclude-standard", "-z")
	if untracked != "" {
		files := strings.Split(untracked, "\x00")
		added := 0
		for _, rel := range files {
			if rel == "" || added >= workspaceMaxUntrackedDiffs {
				continue
			}
			full := filepath.Join(root, filepath.FromSlash(rel))
			info, err := os.Lstat(full)
			if err != nil || !info.Mode().IsRegular() {
				continue
			}
			f, err := os.Open(full)
			if err != nil {
				continue
			}
			data, err := io.ReadAll(io.LimitReader(f, 16<<10))
			f.Close()
			if err != nil || bytes.IndexByte(data, 0) >= 0 {
				continue
			}
			if b.Len() > 0 {
				b.WriteString("\n\n")
			}
			b.WriteString("新文件：" + rel + "\n")
			b.WriteString("--- /dev/null\n+++ b/" + rel + "\n")
			for _, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
				b.WriteString("+" + line + "\n")
			}
			added++
		}
		if added > 0 {
			b.WriteString(fmt.Sprintf("\n（最多展示 %d 个新文件）", workspaceMaxUntrackedDiffs))
		}
	}
	if b.Len() == 0 {
		return "工作区没有未提交的改动。"
	}
	text := b.String()
	if len(text) > m.opts.MaxOutputBytes {
		text = text[:m.opts.MaxOutputBytes] + "\n\n（diff 已截断）"
	}
	return text
}

func (m *WorkspaceManager) sendPath(ctx context.Context, userID, raw string) (string, error) {
	root, _, err := m.currentRoot(ctx, userID, true)
	if err != nil {
		return "", err
	}
	target, err := resolveWorkspacePath(root, raw)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(target)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("不是普通文件；目录请用 /pack")
	}
	if info.Size() > m.opts.MaxArchiveBytes {
		return "", fmt.Errorf("文件太大（%s），上限 %d MB", humanSize(info.Size()), m.opts.MaxArchiveBytes>>20)
	}
	real, err := filepath.EvalSymlinks(target)
	if err != nil || !isWithinRoot(root, real) {
		return "", fmt.Errorf("路径越过工作区边界")
	}
	return real, nil
}

func skipPackDir(name string) bool {
	switch name {
	case ".git", "node_modules", ".venv", "venv", "__pycache__", ".cache":
		return true
	}
	return false
}

func (m *WorkspaceManager) createArchive(ctx context.Context, userID string) (string, string, func(), error) {
	root, name, err := m.currentRoot(ctx, userID, true)
	if err != nil {
		return "", "", nil, err
	}
	tmpDir, err := os.MkdirTemp("", "weclaw-pack-")
	if err != nil {
		return "", "", nil, err
	}
	cleanup := func() { _ = os.RemoveAll(tmpDir) }
	archiveName := fmt.Sprintf("%s-%s.zip", name, time.Now().Format("20060102-150405"))
	archivePath := filepath.Join(tmpDir, archiveName)
	f, err := os.OpenFile(archivePath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		cleanup()
		return "", "", nil, err
	}

	zw := zip.NewWriter(f)
	remaining := m.opts.MaxArchiveBytes
	fileCount := 0
	walkErr := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == root {
			return nil
		}
		if entry.IsDir() {
			if skipPackDir(entry.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		if fileCount >= 5000 {
			return fmt.Errorf("文件数量超过 5000")
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		header, err := zip.FileInfoHeader(info)
		if err != nil {
			return err
		}
		header.Name = filepath.ToSlash(rel)
		header.Method = zip.Deflate
		w, err := zw.CreateHeader(header)
		if err != nil {
			return err
		}
		src, err := os.Open(path)
		if err != nil {
			return err
		}
		lr := &io.LimitedReader{R: src, N: remaining + 1}
		n, copyErr := io.Copy(w, lr)
		src.Close()
		if copyErr != nil {
			return copyErr
		}
		if n > remaining {
			return fmt.Errorf("打包内容超过 %d MB", m.opts.MaxArchiveBytes>>20)
		}
		remaining -= n
		fileCount++
		return nil
	})
	closeErr := zw.Close()
	fileCloseErr := f.Close()
	if walkErr != nil {
		cleanup()
		return "", "", nil, walkErr
	}
	if closeErr != nil {
		cleanup()
		return "", "", nil, closeErr
	}
	if fileCloseErr != nil {
		cleanup()
		return "", "", nil, fileCloseErr
	}
	if fileCount == 0 {
		cleanup()
		return "", "", nil, fmt.Errorf("工作区没有可打包的文件")
	}
	return archivePath, name, cleanup, nil
}

func conversationForWorkspace(userID, name string) string {
	return "workspace:" + userID + ":" + name
}

func (m *WorkspaceManager) ensureAgent(ctx context.Context, userID string) (agent.Agent, string, string, error) {
	if m.factory == nil || m.opts.Agent == "" {
		return nil, "", "", fmt.Errorf("没有配置可用的代码 agent（workspace.agent 或 qwen）")
	}
	root, name, err := m.currentRoot(ctx, userID, true)
	if err != nil {
		return nil, "", "", err
	}
	u, err := m.getUser(userID)
	if err != nil {
		return nil, "", "", err
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.agent == nil {
		ag, err := m.factory(ctx, m.opts.Agent, root)
		if err != nil {
			return nil, "", "", err
		}
		u.agent = ag
		u.agentName = m.opts.Agent
		log.Printf("[workspace] started agent %q for %s at %s", m.opts.Agent, userID, root)
	}
	u.agent.SetCwd(root)
	return u.agent, u.agentName, conversationForWorkspace(userID, name), nil
}

func splitWorkspaceCommand(trimmed string) (string, string, bool) {
	if !strings.HasPrefix(trimmed, "/") {
		return "", "", false
	}
	body := trimmed[1:]
	cmd := body
	rest := ""
	if idx := strings.IndexAny(body, " \t\r\n"); idx >= 0 {
		cmd = body[:idx]
		rest = body[idx+1:]
	}
	switch cmd {
	case "ws", "code", "files", "cat", "write", "run", "diff", "pack", "send":
		return cmd, rest, true
	default:
		return "", "", false
	}
}

func (h *Handler) sendWorkspaceText(ctx context.Context, client *ilink.Client, msg ilink.WeixinMessage, text, clientID string) {
	if strings.TrimSpace(text) == "" {
		return
	}
	if err := SendTextReply(ctx, client, msg.FromUserID, text, msg.ContextToken, clientID); err != nil {
		log.Printf("[workspace] failed to send reply to %s: %v", msg.FromUserID, err)
	}
}

func (h *Handler) handleWorkspaceCommand(ctx context.Context, client *ilink.Client, msg ilink.WeixinMessage, trimmed, clientID string) bool {
	command, rest, ok := splitWorkspaceCommand(trimmed)
	if !ok {
		return false
	}
	if h.workspace == nil {
		h.sendWorkspaceText(ctx, client, msg, "代码工作区未启用。", clientID)
		return true
	}

	switch command {
	case "ws":
		fields := strings.Fields(rest)
		if len(fields) > 0 && fields[0] == "import" {
			source := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(rest), "import"))
			h.handleWorkspaceImport(ctx, client, msg, source, clientID)
			return true
		}
		h.sendWorkspaceText(ctx, client, msg, h.workspace.switchCommand(ctx, msg.FromUserID, rest), clientID)
	case "files":
		sub := strings.TrimSpace(rest)
		if sub == "" {
			sub = "."
		}
		h.sendWorkspaceText(ctx, client, msg, h.workspace.listFiles(ctx, msg.FromUserID, sub), clientID)
	case "cat":
		if strings.TrimSpace(rest) == "" {
			h.sendWorkspaceText(ctx, client, msg, "用法：/cat <相对路径>", clientID)
			return true
		}
		h.sendWorkspaceText(ctx, client, msg, h.workspace.readFile(ctx, msg.FromUserID, rest), clientID)
	case "write":
		arg := strings.TrimLeft(rest, " \t")
		idx := strings.IndexAny(arg, " \t\r\n")
		if idx < 0 {
			h.sendWorkspaceText(ctx, client, msg, "用法：/write <相对路径> <内容>", clientID)
			return true
		}
		path := arg[:idx]
		content := arg[idx+1:]
		if strings.TrimSpace(path) == "" || content == "" {
			h.sendWorkspaceText(ctx, client, msg, "用法：/write <相对路径> <内容>", clientID)
			return true
		}
		h.sendWorkspaceText(ctx, client, msg, h.workspace.writeFile(ctx, msg.FromUserID, path, content), clientID)
	case "run":
		if strings.TrimSpace(rest) == "" {
			h.sendWorkspaceText(ctx, client, msg, "用法：/run <命令>", clientID)
			return true
		}
		taskCtx, finish := h.beginTask(ctx, msg.FromUserID, "执行命令", nil)
		defer finish()
		go func() {
			_ = SendTypingState(ctx, client, msg.FromUserID, msg.ContextToken)
		}()
		output := h.workspace.runCommand(taskCtx, msg.FromUserID, rest)
		if taskCtx.Err() != nil {
			log.Printf("[workspace] run command canceled for %s", msg.FromUserID)
			return true
		}
		h.sendWorkspaceText(ctx, client, msg, output, clientID)
	case "diff":
		go func() {
			_ = SendTypingState(ctx, client, msg.FromUserID, msg.ContextToken)
		}()
		h.sendWorkspaceText(ctx, client, msg, h.workspace.diff(ctx, msg.FromUserID), clientID)
	case "pack":
		archivePath, wsName, cleanup, err := h.workspace.createArchive(ctx, msg.FromUserID)
		if err != nil {
			h.sendWorkspaceText(ctx, client, msg, "打包失败："+err.Error(), clientID)
			return true
		}
		defer cleanup()
		if err := SendMediaFromPath(ctx, client, msg.FromUserID, archivePath, msg.ContextToken); err != nil {
			h.sendWorkspaceText(ctx, client, msg, "打包完成但发送失败："+err.Error(), clientID)
			return true
		}
		h.sendWorkspaceText(ctx, client, msg, fmt.Sprintf("已打包并发送工作区 %s：%s", wsName, filepath.Base(archivePath)), clientID)
	case "send":
		raw := strings.TrimSpace(rest)
		if raw == "" {
			h.sendWorkspaceText(ctx, client, msg, "用法：/send <相对路径>", clientID)
			return true
		}
		target, err := h.workspace.sendPath(ctx, msg.FromUserID, raw)
		if err != nil {
			h.sendWorkspaceText(ctx, client, msg, "发送失败："+err.Error(), clientID)
			return true
		}
		if err := SendMediaFromPath(ctx, client, msg.FromUserID, target, msg.ContextToken); err != nil {
			h.sendWorkspaceText(ctx, client, msg, "发送失败："+err.Error(), clientID)
			return true
		}
		h.sendWorkspaceText(ctx, client, msg, "已发送："+filepath.Base(target), clientID)
	case "code":
		task := strings.TrimSpace(rest)
		if task == "" {
			h.sendWorkspaceText(ctx, client, msg, "用法：/code <任务>", clientID)
			return true
		}
		_, wsName, err := h.workspace.currentRoot(ctx, msg.FromUserID, true)
		if err != nil {
			h.sendWorkspaceText(ctx, client, msg, "工作区不可用："+err.Error(), clientID)
			return true
		}
		go func() {
			_ = SendTypingState(ctx, client, msg.FromUserID, msg.ContextToken)
		}()
		prompt := fmt.Sprintf("你在 WeClaw 代码工作区「%s」中工作，当前目录就是工作区根目录。请直接读写文件、运行测试来完成任务，不要访问工作区之外的路径。完成后简短说明改动和验证结果；需要回传的文件，请提醒我用 /send <相对路径> 取回。\n\n任务：\n%s", wsName, task)
		taskCtx, finish := h.beginTask(ctx, msg.FromUserID, "代码任务", func(stopCtx context.Context) {
			h.workspace.cancelTurn(stopCtx, msg.FromUserID)
		})
		defer finish()
		reply, agentName, err := h.workspace.chat(taskCtx, msg.FromUserID, prompt, h.toolProgressForwarder(ctx, client, msg))
		if taskCtx.Err() != nil {
			log.Printf("[workspace] code task canceled for %s", msg.FromUserID)
			return true
		}
		if err != nil {
			log.Printf("[workspace] agent task failed for %s: %v", msg.FromUserID, err)
			h.sendWorkspaceText(ctx, client, msg, "代码任务失败："+err.Error(), clientID)
			return true
		}
		if strings.TrimSpace(reply) == "" {
			reply = "任务结束，agent 没有返回文字。用 /files 或 /diff 查看工作区。"
		}
		h.sendReplyWithMedia(ctx, client, msg, agentName, reply, clientID)
	}
	return true
}

func (m *WorkspaceManager) cancelTurn(ctx context.Context, userID string) {
	u, err := m.getUser(userID)
	if err != nil {
		return
	}
	u.mu.Lock()
	ag := u.agent
	name := u.current
	if name == "" {
		name = defaultWorkspaceName
	}
	u.mu.Unlock()
	canceller, ok := ag.(agent.TurnCanceler)
	if !ok {
		return
	}
	if err := canceller.CancelTurn(ctx, conversationForWorkspace(userID, name)); err != nil {
		log.Printf("[workspace] cancel agent turn for %s: %v", userID, err)
	}
}

func (m *WorkspaceManager) chat(ctx context.Context, userID, task string, progress func(agent.ToolProgressEvent)) (string, string, error) {
	ag, agentName, conversationID, err := m.ensureAgent(ctx, userID)
	if err != nil {
		return "", "", err
	}
	chatCtx, cancel := context.WithTimeout(ctx, m.opts.Timeout)
	defer cancel()

	var reply string
	switch a := ag.(type) {
	case agent.AttachmentProgressChatAgent:
		reply, err = a.ChatWithAttachmentsAndProgress(chatCtx, conversationID, task, nil, progress)
	case agent.AttachmentChatAgent:
		reply, err = a.ChatWithAttachments(chatCtx, conversationID, task, nil)
	case agent.ProgressChatAgent:
		reply, err = a.ChatWithProgress(chatCtx, conversationID, task, progress)
	default:
		reply, err = ag.Chat(chatCtx, conversationID, task)
	}
	return reply, agentName, err
}
