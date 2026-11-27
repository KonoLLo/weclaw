package messaging

import (
	"context"
	"errors"
	"log"
	"strconv"
	"strings"

	"github.com/fastclaw-ai/weclaw/ilink"
)

func (h *Handler) handleMemoryCommand(ctx context.Context, client *ilink.Client, msg ilink.WeixinMessage, text, clientID string) bool {
	cmd, ok := parseMemoryCommand(text)
	if !ok {
		return false
	}
	if h.memoryStore == nil {
		_ = SendTextReply(ctx, client, msg.FromUserID, "记忆功能没启用。", msg.ContextToken, clientID)
		return true
	}

	reply := h.executeMemoryCommand(msg.FromUserID, cmd)
	if err := SendTextReply(ctx, client, msg.FromUserID, reply, msg.ContextToken, clientID); err != nil {
		log.Printf("[memory] failed to send reply to %s: %v", msg.FromUserID, err)
	}
	return true
}

func (h *Handler) executeMemoryCommand(userID string, cmd memoryCommand) string {
	switch cmd.kind {
	case memoryCmdRemember:
		text := strings.TrimSpace(cmd.arg)
		if text == "" {
			return "用法：/记住 <内容>，最多 300 字。"
		}
		entry, err := h.memoryStore.Add(userID, text)
		if err != nil {
			switch {
			case errors.Is(err, ErrMemorySecret):
				return "这类内容（密码、密钥、证件号等）我不会保存，也不建议发在聊天里。"
			case errors.Is(err, ErrMemoryFull):
				return "记忆满了（最多 " + strconv.Itoa(memoryMaxPerUser) + " 条）。\n\n先用 /忘记 <编号> 删掉一些。"
			case errors.Is(err, ErrMemoryTooLong):
				return "这条太长了，压缩到 300 字以内再记。"
			default:
				log.Printf("[memory] save failed for %s: %v", userID, err)
				return "没存上，稍后再试。"
			}
		}
		log.Printf("[memory] saved note id=%d for %s", entry.ID, userID)
		return "已记住第 " + strconv.FormatInt(entry.ID, 10) + " 条。\n\n" + entry.Text

	case memoryCmdList:
		page := 1
		if arg := strings.TrimSpace(cmd.arg); arg != "" {
			n, err := strconv.Atoi(arg)
			if err != nil {
				return "页码要是数字，例如：/记忆 2"
			}
			page = n
		}
		if page < 1 {
			return "页码从 1 开始。"
		}
		return formatMemoryList(h.memoryStore.List(userID), page)

	case memoryCmdForget:
		arg := strings.TrimSpace(cmd.arg)
		if arg == "" {
			return "用法：/忘记 <编号或关键词>"
		}
		if id, err := strconv.ParseInt(arg, 10, 64); err == nil {
			return h.forgetMemoryByID(userID, id)
		}
		matches := h.memoryStore.Find(userID, arg)
		if len(matches) == 0 {
			return "没找到包含“" + arg + "”的记忆。"
		}
		if len(matches) > 1 {
			return formatMemoryMatches(matches)
		}
		return h.forgetMemoryByID(userID, matches[0].ID)
	}
	return "没看懂这条记忆命令。"
}

func (h *Handler) forgetMemoryByID(userID string, id int64) string {
	entry, found, err := h.memoryStore.Forget(userID, id)
	if err != nil {
		log.Printf("[memory] delete failed for %s: %v", userID, err)
		return "删除没成功，稍后再试。"
	}
	if !found {
		return "没找到编号 " + strconv.FormatInt(id, 10) + "。\n\n发 /记忆 查看编号。"
	}
	log.Printf("[memory] deleted note id=%d for %s", id, userID)
	return "已忘记第 " + strconv.FormatInt(id, 10) + " 条。\n\n" + entry.Text
}

func (h *Handler) memoryPromptFor(userID, query string) string {
	if h.memoryStore == nil {
		return ""
	}
	return formatMemoryPrompt(h.memoryStore.List(userID), query)
}
