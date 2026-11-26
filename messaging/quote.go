package messaging

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"log"
	"mime"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/fastclaw-ai/weclaw/agent"
	"github.com/fastclaw-ai/weclaw/ilink"
	"github.com/google/uuid"
)

// quoteContextForMessage resolves both inline and ID-only quotes. It returns a
// prompt block and any quoted media that should travel with the question.
func (h *Handler) quoteContextForMessage(ctx context.Context, client *ilink.Client, msg ilink.WeixinMessage) (string, []agent.Attachment) {
	item := findQuoteItem(msg)
	if item == nil || item.RefMsg == nil {
		return "", nil
	}
	ref := item.RefMsg
	refID := strings.TrimSpace(ref.SvrID)
	if refID == "" && ref.MessageItem != nil {
		refID = strings.TrimSpace(ref.MessageItem.MsgID)
	}

	record, hasRecord := quoteRecord{}, false
	if client != nil && refID != "" {
		record, hasRecord = lookupQuoteMessage(client.BotID(), msg.FromUserID, refID)
	}

	body := quoteInlineBody(ref)
	mediaPath := ""
	mediaMIME := ""
	mediaName := ""
	if hasRecord {
		if body == "" {
			body = record.Body
		}
		mediaPath = record.MediaPath
		mediaMIME = record.MediaMIME
		mediaName = record.MediaName
		if mediaPath != "" {
			if _, err := os.Stat(mediaPath); err != nil {
				mediaPath = ""
			}
		}
	}

	if ref.MessageItem != nil {
		if path, mimeType, name, ok := h.downloadQuotedMedia(ctx, msg, ref.MessageItem); ok {
			mediaPath, mediaMIME, mediaName = path, mimeType, name
			if client != nil && refID != "" {
				size := int64(0)
				if info, err := os.Stat(path); err == nil {
					size = info.Size()
				}
				rememberQuoteMedia(client.BotID(), msg.FromUserID, "inbound", path, mimeType, name, size, refID)
			}
		} else if body == "" {
			body = mediaPlaceholder(ref.MessageItem)
		}
	}

	if ref.PartialText != nil && body != "" {
		if partial := resolvePartialQuote(body, ref.PartialText); partial != "" {
			body = partial
		}
	}

	body = strings.TrimSpace(body)
	if body == "" && mediaPath == "" {
		if refID != "" {
			return "[用户引用了一条消息]\n本鱼没有取到这条引用的内容（可能太旧或未缓存）。\n[引用结束]", nil
		}
		return "", nil
	}

	var b strings.Builder
	b.WriteString("[用户引用了一条消息]\n")
	if body != "" {
		b.WriteString("引用内容：\n")
		b.WriteString(body)
		b.WriteString("\n")
	}
	if mediaPath != "" {
		name := mediaName
		if name == "" {
			name = filepath.Base(mediaPath)
		}
		fmt.Fprintf(&b, "引用附件：%s（已附加，路径：%s）\n", name, mediaPath)
	}
	b.WriteString("[引用结束]")

	var attachments []agent.Attachment
	if mediaPath != "" {
		if mediaMIME == "" {
			mediaMIME = inferContentType(mediaPath)
		}
		if mediaName == "" {
			mediaName = filepath.Base(mediaPath)
		}
		attachments = append(attachments, agent.Attachment{
			Path:     mediaPath,
			MIMEType: mediaMIME,
			Name:     mediaName,
		})
	}
	return b.String(), attachments
}

func findQuoteItem(msg ilink.WeixinMessage) *ilink.MessageItem {
	for i := range msg.ItemList {
		if msg.ItemList[i].RefMsg != nil {
			return &msg.ItemList[i]
		}
	}
	return nil
}

func quoteInlineBody(ref *ilink.RefMessage) string {
	if ref == nil {
		return ""
	}
	var parts []string
	if title := strings.TrimSpace(ref.Title); title != "" {
		parts = append(parts, title)
	}
	if ref.MessageItem != nil {
		if body := quoteItemBody(ref.MessageItem); body != "" {
			parts = append(parts, body)
		}
	}
	return strings.Join(parts, "\n")
}

func quoteItemBody(item *ilink.MessageItem) string {
	if item == nil {
		return ""
	}
	switch item.Type {
	case ilink.ItemTypeText:
		if item.TextItem != nil {
			return strings.TrimSpace(item.TextItem.Text)
		}
	case ilink.ItemTypeVoice:
		if item.VoiceItem != nil {
			if text := strings.TrimSpace(item.VoiceItem.Text); text != "" {
				return text
			}
			return "[引用的语音]"
		}
	case ilink.ItemTypeImage:
		return "[引用的图片]"
	case ilink.ItemTypeVideo:
		return "[引用的视频]"
	case ilink.ItemTypeFile:
		if item.FileItem != nil && strings.TrimSpace(item.FileItem.FileName) != "" {
			return "[引用的文件：" + strings.TrimSpace(item.FileItem.FileName) + "]"
		}
		return "[引用的文件]"
	}
	return ""
}

func mediaPlaceholder(item *ilink.MessageItem) string {
	switch item.Type {
	case ilink.ItemTypeImage:
		return "[引用的图片已过期或未能下载]"
	case ilink.ItemTypeVideo:
		return "[引用的视频已过期或未能下载]"
	case ilink.ItemTypeFile:
		return "[引用的文件已过期或未能下载]"
	case ilink.ItemTypeVoice:
		return "[引用的语音已过期或未能下载]"
	default:
		return ""
	}
}

func (h *Handler) downloadQuotedMedia(ctx context.Context, msg ilink.WeixinMessage, item *ilink.MessageItem) (string, string, string, bool) {
	if item == nil {
		return "", "", "", false
	}
	saveDir := h.mediaSaveDir()
	ts := time.Now().Format("20060102-150405")
	base := fmt.Sprintf("quote-%s-%s", ts, uuid.New().String()[:8])

	switch item.Type {
	case ilink.ItemTypeImage:
		if item.ImageItem == nil {
			return "", "", "", false
		}
		var data []byte
		var err error
		if item.ImageItem.URL != "" {
			data, _, err = downloadFile(ctx, item.ImageItem.URL)
		} else {
			data, err = DownloadMediaInfo(ctx, item.ImageItem.Media, item.ImageItem.AESKey)
		}
		if err != nil {
			log.Printf("[quote] failed to download quoted image: %v", err)
			return "", "", "", false
		}
		ext := detectImageExt(data)
		path := filepath.Join(saveDir, base+ext)
		if err := os.MkdirAll(saveDir, 0o755); err != nil {
			return "", "", "", false
		}
		if err := os.WriteFile(path, data, 0o644); err != nil {
			log.Printf("[quote] failed to save quoted image: %v", err)
			return "", "", "", false
		}
		return path, mimeTypeFromImageExt(ext), base + ext, true

	case ilink.ItemTypeVideo:
		if item.VideoItem == nil || item.VideoItem.Media == nil {
			return "", "", "", false
		}
		data, err := DownloadMediaInfo(ctx, item.VideoItem.Media, "")
		if err != nil {
			log.Printf("[quote] failed to download quoted video: %v", err)
			return "", "", "", false
		}
		if err := os.MkdirAll(saveDir, 0o755); err != nil {
			return "", "", "", false
		}
		path := filepath.Join(saveDir, base+".mp4")
		if err := os.WriteFile(path, data, 0o644); err != nil {
			return "", "", "", false
		}
		return path, "video/mp4", base + ".mp4", true

	case ilink.ItemTypeFile:
		if item.FileItem == nil || item.FileItem.Media == nil {
			return "", "", "", false
		}
		data, err := DownloadMediaInfo(ctx, item.FileItem.Media, "")
		if err != nil {
			log.Printf("[quote] failed to download quoted file: %v", err)
			return "", "", "", false
		}
		name := sanitizeIncomingFileName(item.FileItem.FileName)
		if name == "" {
			name = "quoted-file"
		}
		path := filepath.Join(saveDir, base+"-"+name)
		if err := os.MkdirAll(saveDir, 0o755); err != nil {
			return "", "", "", false
		}
		if err := os.WriteFile(path, data, 0o644); err != nil {
			return "", "", "", false
		}
		mimeType := mime.TypeByExtension(filepath.Ext(name))
		if mimeType == "" {
			mimeType = "application/octet-stream"
		}
		return path, mimeType, name, true
	}
	return "", "", "", false
}

func resolvePartialQuote(fullText string, partial *ilink.PartialText) string {
	if fullText == "" || partial == nil || partial.Start == "" || partial.End == "" {
		return ""
	}
	candidates := []string{
		partialQuoteCandidate(fullText, partial, false),
		partialQuoteCandidate(fullText, partial, true),
	}
	var unique []string
	for _, candidate := range candidates {
		if candidate == "" {
			continue
		}
		dup := false
		for _, existing := range unique {
			if existing == candidate {
				dup = true
				break
			}
		}
		if !dup {
			unique = append(unique, candidate)
		}
	}
	if partial.QuoteMD5 == "" {
		if len(unique) > 0 {
			return unique[0]
		}
		return ""
	}
	expected := strings.ToLower(strings.TrimSpace(partial.QuoteMD5))
	for _, candidate := range unique {
		sum := md5.Sum([]byte(candidate))
		if hex.EncodeToString(sum[:]) == expected {
			return candidate
		}
	}
	return ""
}

func partialQuoteCandidate(fullText string, partial *ilink.PartialText, relativeEnd bool) string {
	runes := []rune(fullText)
	start := nthRuneIndex(runes, partial.Start, partial.StartIndex, 0)
	if start < 0 {
		return ""
	}
	from := 0
	if relativeEnd {
		from = start + len([]rune(partial.Start))
	}
	end := nthRuneIndex(runes, partial.End, partial.EndIndex, from)
	if end < start {
		return ""
	}
	end += len([]rune(partial.End))
	if end > len(runes) {
		end = len(runes)
	}
	return string(runes[start:end])
}

func nthRuneIndex(text []rune, sub string, occurrence, from int) int {
	if sub == "" || occurrence < 0 || from < 0 || from > len(text) {
		return -1
	}
	needle := []rune(sub)
	if len(needle) == 0 {
		return -1
	}
	current := 0
	for i := from; i+len(needle) <= len(text); i++ {
		match := true
		for j := range needle {
			if text[i+j] != needle[j] {
				match = false
				break
			}
		}
		if !match {
			continue
		}
		if current == occurrence {
			return i
		}
		current++
	}
	return -1
}
