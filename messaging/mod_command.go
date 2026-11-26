package messaging

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/fastclaw-ai/weclaw/agent"
)

// modelSwitcher is the capability /mod needs. Only HTTP agents implement it;
// ACP and CLI agents pick their model inside their own config.
type modelSwitcher interface {
	Models() []string
	CurrentModel() string
	SetModel(string) error
	ProbeModels(context.Context) []agent.ModelProbe
}

// WeChat bubbles fold a single newline into the same paragraph, so every
// visual line break in these replies is a blank line.
func modJoin(lines []string) string {
	return strings.Join(lines, "\n\n")
}

// parseModArgs splits "/mod", "/mod 3", "/mod glm-4.5-flash", "/mod-t", "/mod -t".
func parseModArgs(text string) (sub string, arg string) {
	t := strings.TrimSpace(text)
	lower := strings.ToLower(t)
	switch {
	case lower == "/mod-t" || lower == "/modt" || lower == "/mod test":
		return "-t", ""
	case lower == "/mod":
		return "", ""
	case strings.HasPrefix(lower, "/mod "):
		rest := strings.TrimSpace(t[len("/mod"):])
		if r := strings.ToLower(rest); r == "-t" || r == "t" || r == "test" {
			return "-t", ""
		}
		return "arg", rest
	}
	return "arg", ""
}

func (h *Handler) handleModCommand(ctx context.Context, text string) string {
	sub, arg := parseModArgs(text)
	ag := h.getDefaultAgent()
	if ag == nil {
		return "没有可用的 agent，/mod 用不了。"
	}
	sw, ok := ag.(modelSwitcher)
	if !ok {
		info := ag.Info()
		return fmt.Sprintf("/mod 只对 http 档生效。\n\n当前 agent：%s（type=%s，model=%s）",
			info.Name, info.Type, info.Model)
	}
	pool := sw.Models()
	cur := sw.CurrentModel()

	if sub == "-t" {
		if len(pool) == 0 {
			return fmt.Sprintf("没有配置候选池（models 为空）。\n\n当前模型：%s", cur)
		}
		cctx, cancel := context.WithTimeout(ctx, 90*time.Second)
		defer cancel()
		started := time.Now()
		res := sw.ProbeModels(cctx)
		lines := []string{fmt.Sprintf("模型可用性（%s 那一刻的快照，免费档会分钟级波动）：", started.Format("15:04:05"))}
		for i, r := range res {
			line := fmt.Sprintf("%d) %s → %s（%.1fs）", i+1, r.Model, r.Status, r.Elapsed.Seconds())
			if r.Detail != "" {
				line += " " + r.Detail
			}
			lines = append(lines, line)
		}
		lines = append(lines, "用 /mod <编号> 切换")
		return modJoin(lines)
	}

	if sub == "" {
		if len(pool) == 0 {
			return fmt.Sprintf("没有配置候选池（models 为空）。\n\n当前模型：%s", cur)
		}
		lines := []string{"可选模型："}
		for i, m := range pool {
			mark := ""
			if strings.EqualFold(m, cur) {
				mark = "  ←当前"
			}
			lines = append(lines, fmt.Sprintf("%d) %s%s", i+1, m, mark))
		}
		lines = append(lines, "用 /mod <编号> 切换，/mod-t 检测哪些现在能用")
		return modJoin(lines)
	}

	if len(pool) == 0 {
		return "没有配置候选池（models 为空），无法切换。"
	}
	if n, err := strconv.Atoi(arg); err == nil {
		if n < 1 || n > len(pool) {
			return fmt.Sprintf("编号超出范围：1-%d", len(pool))
		}
		arg = pool[n-1]
	}
	if err := sw.SetModel(arg); err != nil {
		return fmt.Sprintf("切换失败：%v", err)
	}
	return fmt.Sprintf("已切到 %s", sw.CurrentModel())
}
