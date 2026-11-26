package messaging

import (
	"archive/zip"
	"context"
	"fmt"
	"io"
	"log"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/fastclaw-ai/weclaw/ilink"
)

const (
	workspaceImportTimeout  = 5 * time.Minute
	workspaceMaxImportFiles = 5000
	workspaceMaxImportBytes = 100 << 20
	workspaceImportStaging  = ".import-"
)

// normalizeGitHubRepo accepts a public github.com repository URL and returns
// its canonical clone URL plus the repository name.
func normalizeGitHubRepo(raw string) (string, string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", "", fmt.Errorf("缺少 GitHub 链接")
	}
	lower := strings.ToLower(raw)
	if !strings.Contains(lower, "://") {
		if strings.HasPrefix(lower, "github.com/") {
			raw = "https://" + raw
		} else {
			return "", "", fmt.Errorf("只支持 github.com 的公开仓库链接")
		}
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", "", fmt.Errorf("链接格式不对")
	}
	if strings.ToLower(u.Scheme) != "https" || strings.ToLower(u.Host) != "github.com" {
		return "", "", fmt.Errorf("只支持 https://github.com/owner/repo")
	}
	if u.User != nil || u.Port() != "" || u.RawQuery != "" || u.Fragment != "" {
		return "", "", fmt.Errorf("链接里不要带账号、端口、参数或锚点")
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) != 2 {
		return "", "", fmt.Errorf("链接要是 https://github.com/owner/repo")
	}
	owner := parts[0]
	repo := strings.TrimSuffix(parts[1], ".git")
	if !validGitHubPart(owner) || !validGitHubPart(repo) {
		return "", "", fmt.Errorf("仓库名不合法")
	}
	if repo == "" {
		return "", "", fmt.Errorf("链接里没有仓库名")
	}
	return "https://github.com/" + owner + "/" + repo, repo, nil
}

func validGitHubPart(s string) bool {
	if s == "" || s == "." || s == ".." || len(s) > 100 {
		return false
	}
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
			continue
		}
		return false
	}
	return true
}

func workspaceNameFromRepo(repo string) string {
	name := strings.TrimSpace(repo)
	name = strings.TrimSuffix(name, ".git")
	var b strings.Builder
	for _, r := range name {
		if unicodeLetterOrDigit(r) || r == '-' || r == '_' || r == '.' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	name = strings.Trim(b.String(), "-.")
	if name == "" {
		name = "imported"
	}
	if len(name) > 48 {
		name = name[:48]
	}
	if !validWorkspaceName(name) {
		name = "imported-" + time.Now().Format("20060102-150405")
	}
	return name
}

func unicodeLetterOrDigit(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r > 127
}

func (m *WorkspaceManager) uniqueWorkspaceName(u *workspaceUser, base string) string {
	for i := 1; i < 1000; i++ {
		name := base
		if i > 1 {
			name = fmt.Sprintf("%s-%d", base, i)
		}
		if _, err := os.Lstat(filepath.Join(u.root, name)); os.IsNotExist(err) {
			return name
		}
	}
	return fmt.Sprintf("%s-%d", base, time.Now().Unix())
}

// importWorkspace creates a new workspace from a public GitHub repository or a
// zip upload. Existing workspaces are never overwritten.
func (m *WorkspaceManager) importWorkspace(ctx context.Context, userID, source, archivePath, archiveName string) string {
	u, err := m.getUser(userID)
	if err != nil {
		return "工作区打不开：" + err.Error()
	}
	tmpRoot, err := os.MkdirTemp(u.root, workspaceImportStaging)
	if err != nil {
		return "创建临时目录失败：" + err.Error()
	}
	defer os.RemoveAll(tmpRoot)
	staging := filepath.Join(tmpRoot, "repo")

	var baseName, label string
	var files int
	var size int64
	if strings.TrimSpace(source) != "" {
		repoURL, repo, err := normalizeGitHubRepo(source)
		if err != nil {
			return "导入失败：" + err.Error()
		}
		baseName = workspaceNameFromRepo(repo)
		label = repoURL
		files, size, err = m.cloneRepository(ctx, repoURL, staging)
		if err != nil {
			if ctx.Err() != nil {
				return "导入已停止。"
			}
			return "克隆失败：" + err.Error()
		}
	} else {
		if archivePath == "" {
			return "先发一个 zip 压缩包，或者发送 /ws import <公开 GitHub 链接>。"
		}
		if !strings.EqualFold(filepath.Ext(archiveName), ".zip") {
			return "只能导入 zip 压缩包。"
		}
		baseName = workspaceNameFromRepo(strings.TrimSuffix(filepath.Base(archiveName), filepath.Ext(archiveName)))
		label = archiveName
		var err error
		files, size, err = m.extractZip(ctx, archivePath, staging)
		if err != nil {
			if ctx.Err() != nil {
				return "导入已停止。"
			}
			return "解压失败：" + err.Error()
		}
		if err := flattenSingleDirectory(staging); err != nil {
			return "整理工作区失败：" + err.Error()
		}
	}

	if err := rejectWorkspaceSymlinks(staging); err != nil {
		return "导入失败：" + err.Error()
	}
	files, size, err = workspaceTreeStats(staging)
	if err != nil {
		return "统计工作区失败：" + err.Error()
	}
	if files == 0 {
		return "导入失败：里面没有文件。"
	}
	if files > workspaceMaxImportFiles {
		return fmt.Sprintf("导入失败：文件数量超过 %d。", workspaceMaxImportFiles)
	}
	if limit := m.importByteLimit(); size > limit {
		return fmt.Sprintf("导入失败：文件总大小超过 %d MB。", limit>>20)
	}
	if err := ctx.Err(); err != nil {
		return "导入已停止。"
	}

	name := m.uniqueWorkspaceName(u, baseName)
	target := filepath.Join(u.root, name)
	if err := os.Rename(staging, target); err != nil {
		return "保存工作区失败：" + err.Error()
	}
	initWorkspaceGit(target)

	u.mu.Lock()
	u.current = name
	if u.agent != nil {
		u.agent.SetCwd(target)
	}
	u.mu.Unlock()

	log.Printf("[workspace] imported %q into %s (%s, %d files, %d bytes)", label, target, userID, files, size)
	return fmt.Sprintf("已导入工作区 %s：%d 个文件，%s。\n来源：%s\n接下来直接 /code <任务> 就能开工。", name, files, humanSize(size), label)
}

func (m *WorkspaceManager) cloneRepository(ctx context.Context, repoURL, target string) (int, int64, error) {
	git, err := exec.LookPath("git")
	if err != nil {
		return 0, 0, fmt.Errorf("服务器没有安装 git")
	}
	cloneCtx, cancel := context.WithTimeout(ctx, workspaceImportTimeout)
	defer cancel()
	cmd := exec.CommandContext(cloneCtx, git, "clone", "--depth", "1", "--single-branch", "--", repoURL, target)
	cmd.Dir = filepath.Dir(target)
	cmd.Env = gitEnvForRepo(repoURL)
	if out, err := cmd.CombinedOutput(); err != nil {
		if ctx.Err() != nil {
			return 0, 0, ctx.Err()
		}
		if cloneCtx.Err() != nil {
			return 0, 0, fmt.Errorf("克隆超时（%s）", workspaceImportTimeout)
		}
		detail := lastNonEmptyLine(string(out))
		if detail == "" {
			detail = err.Error()
		}
		return 0, 0, fmt.Errorf("%s", truncate(detail, 300))
	}
	return workspaceTreeStats(target)
}

func gitEnvForRepo(repoURL string) []string {
	env := gitEnv()
	token := strings.TrimSpace(os.Getenv("WECLAW_GITHUB_TOKEN"))
	if token == "" || !strings.HasPrefix(repoURL, "https://github.com/") {
		return env
	}
	return append(env,
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=http.https://github.com/.extraheader",
		"GIT_CONFIG_VALUE_0=Authorization: Bearer "+token,
	)
}

func lastNonEmptyLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if line := strings.TrimSpace(lines[i]); line != "" {
			return line
		}
	}
	return ""
}

func (m *WorkspaceManager) importByteLimit() int64 {
	limit := int64(workspaceMaxImportBytes)
	if m.opts.MaxArchiveBytes > 0 && m.opts.MaxArchiveBytes < limit {
		limit = m.opts.MaxArchiveBytes
	}
	return limit
}

func (m *WorkspaceManager) extractZip(ctx context.Context, archivePath, dest string) (int, int64, error) {
	zr, err := zip.OpenReader(archivePath)
	if err != nil {
		return 0, 0, fmt.Errorf("不是有效的 zip：%w", err)
	}
	defer zr.Close()
	if err := os.MkdirAll(dest, 0o700); err != nil {
		return 0, 0, err
	}
	limit := m.importByteLimit()
	var total int64
	files := 0
	for _, f := range zr.File {
		if err := ctx.Err(); err != nil {
			return 0, 0, err
		}
		name := strings.ReplaceAll(f.Name, "\\", "/")
		if name == "" || strings.HasSuffix(name, "/") {
			continue
		}
		if hasDotDotSegment(name) || strings.HasPrefix(name, "/") || filepath.IsAbs(name) || filepath.VolumeName(name) != "" {
			return 0, 0, fmt.Errorf("压缩包里有不安全路径：%s", f.Name)
		}
		clean := path.Clean(name)
		if clean == "." || clean == "" {
			continue
		}
		if skipImportPath(clean) {
			continue
		}
		if !validImportPath(clean) {
			return 0, 0, fmt.Errorf("压缩包里有不支持的路径：%s", f.Name)
		}
		mode := f.Mode()
		if mode&os.ModeSymlink != 0 {
			return 0, 0, fmt.Errorf("压缩包里不允许符号链接：%s", f.Name)
		}
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(filepath.Join(dest, filepath.FromSlash(clean)), 0o755); err != nil {
				return 0, 0, err
			}
			continue
		}
		if !mode.IsRegular() {
			return 0, 0, fmt.Errorf("压缩包里有特殊文件：%s", f.Name)
		}
		files++
		if files > workspaceMaxImportFiles {
			return 0, 0, fmt.Errorf("文件数量超过 %d", workspaceMaxImportFiles)
		}
		target := filepath.Join(dest, filepath.FromSlash(clean))
		if !isWithinRoot(dest, target) {
			return 0, 0, fmt.Errorf("压缩包路径越界：%s", f.Name)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return 0, 0, err
		}
		rc, err := f.Open()
		if err != nil {
			return 0, 0, err
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
		if err != nil {
			rc.Close()
			return 0, 0, err
		}
		limited := &io.LimitedReader{R: rc, N: limit - total + 1}
		n, copyErr := io.Copy(out, limited)
		closeErr := out.Close()
		rc.Close()
		if copyErr != nil {
			return 0, 0, copyErr
		}
		if closeErr != nil {
			return 0, 0, closeErr
		}
		total += n
		if total > limit {
			return 0, 0, fmt.Errorf("解压后超过 %d MB", limit>>20)
		}
		if mode&0o111 != 0 {
			_ = os.Chmod(target, 0o755)
		}
	}
	if files == 0 {
		return 0, 0, fmt.Errorf("压缩包里没有文件")
	}
	log.Printf("[workspace] extracted %s to %s (%d files, %d bytes)", filepath.Base(archivePath), dest, files, total)
	return files, total, nil
}

func hasDotDotSegment(name string) bool {
	for _, part := range strings.Split(name, "/") {
		if part == ".." {
			return true
		}
	}
	return false
}

func skipImportPath(name string) bool {
	for _, part := range strings.Split(name, "/") {
		if part == ".git" || part == "__MACOSX" {
			return true
		}
	}
	return false
}

func validImportPath(name string) bool {
	for _, part := range strings.Split(name, "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return true
}

// flattenSingleDirectory turns a GitHub-style archive's single top-level folder
// into the workspace root.
func flattenSingleDirectory(root string) error {
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	if len(entries) != 1 || !entries[0].IsDir() {
		return nil
	}
	inner := filepath.Join(root, entries[0].Name())
	children, err := os.ReadDir(inner)
	if err != nil {
		return err
	}
	for _, child := range children {
		from := filepath.Join(inner, child.Name())
		to := filepath.Join(root, child.Name())
		if _, err := os.Lstat(to); err == nil {
			return fmt.Errorf("顶层目录里有重名文件：%s", child.Name())
		}
		if err := os.Rename(from, to); err != nil {
			return err
		}
	}
	return os.Remove(inner)
}

func rejectWorkspaceSymlinks(root string) error {
	return filepath.WalkDir(root, func(p string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			rel, _ := filepath.Rel(root, p)
			return fmt.Errorf("不允许符号链接：%s", filepath.ToSlash(rel))
		}
		return nil
	})
}

func workspaceTreeStats(root string) (int, int64, error) {
	files := 0
	var total int64
	err := filepath.WalkDir(root, func(p string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if p != root && entry.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		files++
		total += info.Size()
		return nil
	})
	return files, total, err
}

func (h *Handler) latestZipUpload(userID string) (pendingIncomingFile, bool) {
	if p, ok := h.currentPendingIncomingFile(userID); ok && strings.EqualFold(filepath.Ext(p.Name), ".zip") {
		return p, true
	}
	return h.findRecentIncomingFileWithExt(userID, ".zip")
}

func (h *Handler) handleWorkspaceImport(ctx context.Context, client *ilink.Client, msg ilink.WeixinMessage, source, clientID string) {
	archivePath, archiveName := "", ""
	if strings.TrimSpace(source) == "" {
		p, ok := h.latestZipUpload(msg.FromUserID)
		if !ok {
			h.sendWorkspaceText(ctx, client, msg, "先发一个 zip 压缩包，或者发送 /ws import <公开 GitHub 链接>。", clientID)
			return
		}
		archivePath = p.Path
		archiveName = p.Name
	}
	taskCtx, finish := h.beginTask(ctx, msg.FromUserID, "导入工作区", nil)
	defer finish()
	go func() {
		_ = SendTypingState(ctx, client, msg.FromUserID, msg.ContextToken)
	}()
	reply := h.workspace.importWorkspace(taskCtx, msg.FromUserID, strings.TrimSpace(source), archivePath, archiveName)
	if taskCtx.Err() != nil {
		log.Printf("[workspace] import canceled for %s", msg.FromUserID)
		return
	}
	h.sendWorkspaceText(ctx, client, msg, reply, clientID)
}
