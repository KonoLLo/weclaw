package messaging

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"mime"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/fastclaw-ai/weclaw/ilink"
	"github.com/google/uuid"
)

const (
	incomingTextMaxBytes  = 256 << 10 // read at most 256 KiB of a text file
	incomingTextMaxChars  = 60000     // and at most 60k characters
	pendingIncomingTTL    = 30 * time.Minute
	pendingIncomingWait   = 2 * time.Second
	recentFileFallbackAge = 30 * time.Minute
)

type pendingIncomingFile struct {
	Name      string
	Path      string
	Content   string
	Kind      string // "" = plain text, "word", "excel"
	ReadErr   string // friendly reason when Word/Excel extraction failed
	Truncated bool
	Received  time.Time
}

var textFileExts = map[string]bool{
	".txt": true, ".text": true, ".md": true, ".markdown": true,
	".go": true, ".py": true, ".js": true, ".mjs": true, ".cjs": true,
	".ts": true, ".tsx": true, ".jsx": true, ".vue": true, ".svelte": true,
	".json": true, ".jsonc": true, ".yaml": true, ".yml": true, ".toml": true,
	".ini": true, ".cfg": true, ".conf": true, ".env": true,
	".sh": true, ".bash": true, ".zsh": true, ".ps1": true, ".bat": true, ".cmd": true,
	".c": true, ".h": true, ".cc": true, ".cpp": true, ".cxx": true, ".hpp": true,
	".java": true, ".kt": true, ".kts": true, ".rs": true, ".rb": true, ".php": true,
	".swift": true, ".sql": true, ".html": true, ".htm": true, ".css": true,
	".scss": true, ".less": true, ".xml": true, ".csv": true, ".tsv": true,
	".log": true, ".diff": true, ".patch": true, ".gitignore": true, ".dockerignore": true,
}

var binaryFileExts = map[string]bool{
	".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".webp": true,
	".bmp": true, ".ico": true, ".pdf": true, ".doc": true, ".docx": true,
	".xls": true, ".xlsx": true, ".ppt": true, ".pptx": true, ".zip": true,
	".gz": true, ".tar": true, ".7z": true, ".rar": true, ".mp3": true,
	".wav": true, ".amr": true, ".silk": true, ".mp4": true, ".mov": true,
	".avi": true, ".mkv": true, ".exe": true, ".dll": true, ".so": true,
	".bin": true, ".class": true, ".jar": true, ".wasm": true,
}

// isTextFile decides whether an inbound file's body is worth passing to the
// model. Known source extensions win; unknown ones are sniffed for UTF-8 text.
func isTextFile(name string, data []byte) bool {
	ext := strings.ToLower(filepath.Ext(name))
	if binaryFileExts[ext] {
		return false
	}
	if textFileExts[ext] {
		return true
	}
	switch strings.ToLower(filepath.Base(name)) {
	case "makefile", "dockerfile", ".gitignore", ".env", ".editorconfig":
		return true
	}
	return looksLikeUTF8Text(data)
}

func looksLikeUTF8Text(data []byte) bool {
	if len(data) == 0 {
		return true
	}
	if bytes.IndexByte(data, 0) >= 0 || !utf8.Valid(data) {
		return false
	}
	printable := 0
	for _, r := range string(data) {
		if r == '\n' || r == '\r' || r == '\t' || r >= 0x20 {
			printable++
		}
	}
	total := utf8.RuneCount(data)
	return total > 0 && printable*10 >= total*9
}

// decodeIncomingText returns the file body, capped so one upload cannot blow up
// the prompt. The bool reports truncation.
func decodeIncomingText(data []byte) (string, bool) {
	truncated := false
	raw := data
	if len(raw) > incomingTextMaxBytes {
		raw = raw[:incomingTextMaxBytes]
		truncated = true
	}
	raw = bytes.ToValidUTF8(raw, []byte(""))
	s := strings.TrimPrefix(string(raw), "\ufeff")
	if runes := []rune(s); len(runes) > incomingTextMaxChars {
		s = string(runes[:incomingTextMaxChars])
		truncated = true
	}
	return s, truncated
}

// handleIncomingFile saves an upload to the workspace and remembers its text so
// the next question about it can reach an HTTP model, which has no filesystem
// access of its own.
func (h *Handler) handleIncomingFile(ctx context.Context, client *ilink.Client, msg ilink.WeixinMessage, fileItem *ilink.FileItem, ack bool) {
	clientID := NewClientID()
	saveDir := h.mediaSaveDir()
	log.Printf("[handler] received file %q from %s, saving to %s", fileItem.FileName, msg.FromUserID, saveDir)

	if fileItem.Media == nil || (fileItem.Media.EncryptQueryParam == "" && fileItem.Media.FullURL == "") {
		log.Printf("[handler] file has no media info from %s", msg.FromUserID)
		if ack {
			_ = SendTextReply(ctx, client, msg.FromUserID, "这个文件没有下载信息，本鱼收不到。再发一次试试。", msg.ContextToken, clientID)
		}
		return
	}

	data, err := DownloadMediaInfo(ctx, fileItem.Media, "")
	if err != nil {
		log.Printf("[handler] failed to download file from %s: %v", msg.FromUserID, err)
		if ack {
			_ = SendTextReply(ctx, client, msg.FromUserID, "文件没接住："+err.Error(), msg.ContextToken, clientID)
		}
		return
	}

	displayName := strings.TrimSpace(fileItem.FileName)
	if displayName == "" {
		displayName = "未命名文件"
	}
	fileName := sanitizeIncomingFileName(fileItem.FileName)
	if fileName == "" {
		fileName = fmt.Sprintf("file_%s", time.Now().Format("20060102-150405"))
	}
	fileName = fmt.Sprintf("%s_%s", time.Now().Format("150405"), fileName)
	filePath := filepath.Join(saveDir, fileName)

	if err := os.MkdirAll(saveDir, 0o755); err != nil {
		log.Printf("[handler] failed to create save dir: %v", err)
		return
	}
	if err := os.WriteFile(filePath, data, 0o644); err != nil {
		log.Printf("[handler] failed to write file: %v", err)
		if ack {
			_ = SendTextReply(ctx, client, msg.FromUserID, "文件存不下来："+err.Error(), msg.ContextToken, clientID)
		}
		return
	}

	sidecarPath := filePath + ".sidecar.md"
	sidecarContent := fmt.Sprintf("---\nid: %s\nfrom_user_id: %s\nmessage_id: %d\n---\n", uuid.New().String(), msg.FromUserID, msg.MessageID)
	if err := os.WriteFile(sidecarPath, []byte(sidecarContent), 0o644); err != nil {
		log.Printf("[handler] failed to write sidecar: %v", err)
	}

	textFile := isTextFile(displayName, data)
	extraction := extractIncomingContent(displayName, data)
	content := extraction.Content
	truncated := extraction.Truncated
	kind := extraction.Kind
	encodingProblem := extraction.EncodingProblem
	readErr := ""
	if extraction.Err != nil {
		readErr = officeReadError(kind)
		log.Printf("[handler] failed to extract %s body: %v", kind, extraction.Err)
	}
	empty := readErr == "" && content == "" && (kind != "" || (textFile && !encodingProblem))

	log.Printf("[handler] saved incoming file to %s (%d bytes, text=%t, kind=%q, encoding_error=%t, read_error=%t, chars=%d, truncated=%t)", filePath, len(data), textFile, kind, encodingProblem, readErr != "", utf8.RuneCountInString(content), truncated)
	rememberInboundMedia(client.BotID(), msg, filePath, mime.TypeByExtension(filepath.Ext(displayName)), displayName)

	h.pendingFiles.Store(msg.FromUserID, pendingIncomingFile{
		Name:      displayName,
		Path:      filePath,
		Content:   content,
		Kind:      kind,
		ReadErr:   readErr,
		Truncated: truncated,
		Received:  time.Now(),
	})

	if !ack {
		return
	}

	var reply string
	ext := strings.ToLower(filepath.Ext(displayName))
	switch {
	case readErr != "":
		reply = fmt.Sprintf("收到《%s》（%s），%s。", displayName, formatIncomingSize(len(data)), readErr)
	case empty && kind != "":
		reply = fmt.Sprintf("收到《%s》（%s），但里面没读到文字（可能是空文档，或只有图片/图表）。", displayName, formatIncomingSize(len(data)))
	case empty:
		reply = fmt.Sprintf("收到《%s》，但文件里是空的。", displayName)
	case content != "" && truncated && kind != "":
		reply = fmt.Sprintf("收到《%s》（%s），%s比较长，本鱼先读了前 %d 字。接下来直接说要查什么就行。", displayName, formatIncomingSize(len(data)), officeKindLabel(kind), incomingTextMaxChars)
	case content != "" && truncated:
		reply = fmt.Sprintf("收到《%s》（%s），文件比较长，本鱼先读了前 %d 字。接下来直接说要查什么就行。", displayName, formatIncomingSize(len(data)), incomingTextMaxChars)
	case content != "" && kind != "":
		reply = fmt.Sprintf("收到《%s》（%s），%s已经读进来了。接下来直接说要查什么就行，比如“帮我总结一下这份文档”。", displayName, formatIncomingSize(len(data)), officeKindLabel(kind))
	case content != "":
		reply = fmt.Sprintf("收到《%s》（%s），正文已经读进来了。接下来直接说要查什么就行，比如“帮我看看这段代码”。", displayName, formatIncomingSize(len(data)))
	case encodingProblem:
		reply = fmt.Sprintf("收到《%s》（%s），但这不是 UTF-8 编码，本鱼读出来是乱码。用记事本另存为 UTF-8 再发一次吧。", displayName, formatIncomingSize(len(data)))
	case ext == ".zip":
		reply = fmt.Sprintf("收到《%s》（%s）。回复 /ws import 可以直接把它导入代码工作区。", displayName, formatIncomingSize(len(data)))
	case ext == ".doc":
		reply = fmt.Sprintf("收到《%s》（%s），这是旧版 Word（.doc）格式，本鱼读不了。用 Office/WPS 另存为 .docx 再发一次。", displayName, formatIncomingSize(len(data)))
	case ext == ".xls":
		reply = fmt.Sprintf("收到《%s》（%s），这是旧版 Excel（.xls）格式，本鱼读不了。用 Office/WPS 另存为 .xlsx 再发一次。", displayName, formatIncomingSize(len(data)))
	case ext == ".pdf":
		reply = fmt.Sprintf("收到《%s》（%s），PDF 本鱼不支持。先转成 .docx 或 txt 再发一次。", displayName, formatIncomingSize(len(data)))
	default:
		reply = fmt.Sprintf("收到《%s》（%s），已经存到服务器。这类文件本鱼读不了正文；如果是代码或文本，另存为 txt 或代码文件再发一次。", displayName, formatIncomingSize(len(data)))
	}
	if err := SendTextReply(ctx, client, msg.FromUserID, reply, msg.ContextToken, clientID); err != nil {
		log.Printf("[handler] failed to send file ack to %s: %v", msg.FromUserID, err)
	}
}

// currentPendingIncomingFile returns the last upload for this user without
// consuming it. A retried or duplicated question must still see the body, and
// HTTP history alone is not enough when the first reply is slow or lost.
func (h *Handler) currentPendingIncomingFile(userID string) (pendingIncomingFile, bool) {
	v, ok := h.pendingFiles.Load(userID)
	if !ok {
		return pendingIncomingFile{}, false
	}
	p := v.(pendingIncomingFile)
	if time.Since(p.Received) > pendingIncomingTTL {
		h.pendingFiles.Delete(userID)
		return pendingIncomingFile{}, false
	}
	return p, true
}

// attachPendingIncomingFile folds the uploaded body into the question that
// refers to it. HTTP agents only get text, so the body itself must travel with
// the prompt; agents with file tools get the path instead.
func (h *Handler) attachPendingIncomingFile(userID, agentType, message string) string {
	if !looksLikeFileQuestion(message) {
		return message
	}
	p, ok := h.currentPendingIncomingFile(userID)
	if !ok {
		// The text can overtake the file handler when both arrive in one poll;
		// give the download a moment to land before answering without it.
		deadline := time.Now().Add(pendingIncomingWait)
		for time.Now().Before(deadline) {
			time.Sleep(100 * time.Millisecond)
			if p, ok = h.currentPendingIncomingFile(userID); ok {
				break
			}
		}
	}
	if !ok {
		// A restart or a lost in-memory entry should not hide a file that was
		// already saved; recover the newest matching upload from its sidecar.
		if p, ok = h.findRecentIncomingFile(userID); ok {
			h.pendingFiles.Store(userID, p)
		}
	}
	if !ok {
		return message
	}
	log.Printf("[handler] attaching uploaded file %q (%d chars) to question from %s", p.Name, utf8.RuneCountInString(p.Content), userID)
	return formatIncomingFilePrompt(p, agentType, message)
}

// findRecentIncomingFile recovers an upload saved by an earlier process. The
// sidecar written next to every file carries the sender, so one user cannot
// pick up another user's file.
func (h *Handler) findRecentIncomingFile(userID string) (pendingIncomingFile, bool) {
	return h.findRecentIncomingFileWithExt(userID, "")
}

func (h *Handler) findRecentIncomingFileWithExt(userID, ext string) (pendingIncomingFile, bool) {
	dir := h.mediaSaveDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return pendingIncomingFile{}, false
	}
	var best pendingIncomingFile
	var bestMod time.Time
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".sidecar.md") {
			continue
		}
		if ext != "" && !strings.HasSuffix(strings.ToLower(strings.TrimSuffix(name, ".sidecar.md")), strings.ToLower(ext)) {
			continue
		}
		sidecarInfo, err := entry.Info()
		if err != nil || time.Since(sidecarInfo.ModTime()) > recentFileFallbackAge {
			continue
		}
		sidecar, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || !strings.Contains(string(sidecar), "from_user_id: "+userID+"\n") {
			continue
		}
		path := filepath.Join(dir, strings.TrimSuffix(name, ".sidecar.md"))
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() || time.Since(info.ModTime()) > recentFileFallbackAge {
			continue
		}
		if !info.ModTime().After(bestMod) {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		displayName := displayNameFromStoredFile(name)
		extraction := extractIncomingContent(displayName, data)
		readErr := ""
		if extraction.Err != nil {
			readErr = officeReadError(extraction.Kind)
		}
		best = pendingIncomingFile{
			Name:      displayName,
			Path:      path,
			Content:   extraction.Content,
			Kind:      extraction.Kind,
			ReadErr:   readErr,
			Truncated: extraction.Truncated,
			Received:  info.ModTime(),
		}
		bestMod = info.ModTime()
	}
	return best, best.Path != ""
}

// displayNameFromStoredFile strips the HHMMSS_ prefix added to saved uploads.
func displayNameFromStoredFile(stored string) string {
	base := strings.TrimSuffix(filepath.Base(stored), ".sidecar.md")
	if len(base) > 7 && base[6] == '_' {
		digits := true
		for _, r := range base[:6] {
			if r < '0' || r > '9' {
				digits = false
				break
			}
		}
		if digits {
			return base[7:]
		}
	}
	return base
}

func formatIncomingFilePrompt(p pendingIncomingFile, agentType, message string) string {
	var b strings.Builder
	embed := strings.EqualFold(agentType, "http") && p.Content != ""
	fmt.Fprintf(&b, "[用户上传的文件：%s", p.Name)
	if name := officeKindName(p.Kind); name != "" {
		fmt.Fprintf(&b, "（%s）", name)
	}
	b.WriteString("]\n")
	if embed {
		if p.Truncated {
			b.WriteString("【文件较长，只读入了前面一部分】\n")
		}
		b.WriteString("--- 文件内容开始 ---\n")
		b.WriteString(p.Content)
		if !strings.HasSuffix(p.Content, "\n") {
			b.WriteString("\n")
		}
		b.WriteString("--- 文件内容结束 ---\n\n")
	} else {
		if p.Content == "" {
			if p.ReadErr != "" {
				fmt.Fprintf(&b, "【%s；文件保存在服务器】\n", p.ReadErr)
			} else {
				b.WriteString("【本鱼没有读到正文，文件保存在服务器】\n")
			}
		}
		fmt.Fprintf(&b, "路径：%s\n\n", p.Path)
	}
	b.WriteString(message)
	return b.String()
}

func looksLikeFileQuestion(message string) bool {
	low := strings.ToLower(message)
	for _, hint := range []string{
		"代码", "文件", "附件", "文档", "内容", "这段", "这个", "这份", "上面", "上述",
		"表格", "工作表", "数据", "看看", "看下", "看一下", "分析", "解释", "检查", "审查", "优化", "修改", "修复",
		"报错", "错误", "问题", "不对",
		"code", "file", "attach", "above", "review", "analyze", "analyse", "explain",
		"check", "bug", "error", "fix", "refactor", "word", "excel", "xlsx", "docx",
	} {
		if strings.Contains(low, hint) {
			return true
		}
	}
	return false
}

func formatIncomingSize(n int) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%dB", n)
	case n < 1024*1024:
		return fmt.Sprintf("%.1fK", float64(n)/1024)
	default:
		return fmt.Sprintf("%.1fM", float64(n)/(1024*1024))
	}
}
