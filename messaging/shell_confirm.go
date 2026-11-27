package messaging

import (
	"context"
	"log"
	"strings"

	"github.com/fastclaw-ai/weclaw/agent"
	"github.com/fastclaw-ai/weclaw/ilink"
)

// shellConfirmationVerdict maps a short owner reply to a staged shell command.
// Matching is exact so an ordinary sentence containing "确认" never fires one.
func shellConfirmationVerdict(text string) string {
	switch strings.ToLower(strings.TrimSpace(text)) {
	case "确认", "确认执行", "confirm":
		return "run"
	case "取消", "算了", "cancel", "不执行":
		return "cancel"
	}
	return ""
}

// buildShellConfirmationPrompt is generated here, not by the model, so a
// "已执行" narration can never slip through when the command was only staged.
func buildShellConfirmationPrompt(command string) string {
	return "【需要确认】这条命令还没有执行：\n\n" + command + "\n\n回复「确认」执行；回复「取消」作废（5 分钟内有效）。"
}

// handleShellConfirmation runs (or drops) a destructive command the model
// staged in server_shell. The owner's reply decides; the model is bypassed.
func (h *Handler) handleShellConfirmation(ctx context.Context, client *ilink.Client, msg ilink.WeixinMessage, trimmed, clientID string) bool {
	verdict := shellConfirmationVerdict(trimmed)
	if verdict == "" {
		return false
	}
	command, ok := agent.TakeShellPending(msg.FromUserID)
	if !ok {
		return false
	}

	if verdict == "cancel" {
		reply := "已取消，不会执行：\n" + command
		if err := SendTextReply(ctx, client, msg.FromUserID, reply, msg.ContextToken, clientID); err != nil {
			log.Printf("[shell] failed to send cancel reply to %s: %v", msg.FromUserID, err)
		}
		return true
	}

	ag := h.getDefaultAgent()
	var shell *agent.ServerShell
	if provider, ok := ag.(interface{ Shell() *agent.ServerShell }); ok {
		shell = provider.Shell()
	}
	if shell == nil {
		_ = SendTextReply(ctx, client, msg.FromUserID, "确认失败：当前助手没有可用的命令通道", msg.ContextToken, clientID)
		return true
	}

	log.Printf("[shell] owner confirmed: %s", truncate(command, 200))
	out, err := shell.RunConfirmed(ctx, command)
	reply := "已执行：\n" + command
	if strings.TrimSpace(out) != "" {
		reply += "\n\n" + out
	}
	if err != nil {
		reply += "\n\n[" + err.Error() + "]"
	}
	if err := SendTextReply(ctx, client, msg.FromUserID, reply, msg.ContextToken, clientID); err != nil {
		log.Printf("[shell] failed to send confirmed result to %s: %v", msg.FromUserID, err)
	}
	if appender, ok := ag.(interface {
		AppendHistory(string, string, string)
	}); ok {
		appender.AppendHistory(msg.FromUserID, "确认", reply)
	}
	return true
}
