package messaging

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/fastclaw-ai/weclaw/agent"
)

// searchEngineSwitcher is the capability /se needs. SearchPool implements it;
// a bare Searcher (tests, single-provider builds) does not.
type searchEngineSwitcher interface {
	SearchEngines() []agent.SearchEngineInfo
	CurrentSearchEngine() string
	SetSearchEngine(string) error
}

var searchEngineCommands = []string{"/se", "/sengine", "/搜源"}

// parseSearchEngineArgs splits "/se", "/se 2", "/se tavily".
func parseSearchEngineArgs(text string) (arg string, ok bool) {
	t := strings.TrimSpace(text)
	low := strings.ToLower(t)
	for _, c := range searchEngineCommands {
		lc := strings.ToLower(c)
		if low == lc {
			return "", true
		}
		if strings.HasPrefix(low, lc+" ") {
			return strings.TrimSpace(t[len(lc):]), true
		}
	}
	return "", false
}

func (h *Handler) handleSearchEngineCommand(text string) string {
	if h.webSearch == nil {
		return "联网搜索没启用。"
	}
	arg, ok := parseSearchEngineArgs(text)
	if !ok {
		return "用法：/se 查看，/se <编号> 切换"
	}
	sw, ok := h.webSearch.(searchEngineSwitcher)
	if !ok {
		return fmt.Sprintf("当前搜索源不支持切换：%s", h.webSearch.Name())
	}
	engines := sw.SearchEngines()

	if arg == "" || strings.EqualFold(arg, "list") || arg == "列表" {
		lines := []string{"搜索源："}
		for i, e := range engines {
			line := fmt.Sprintf("%d) %s", i+1, e.Name)
			if e.Available {
				if e.Current {
					line += "  ←当前"
				}
			} else {
				reason := e.Detail
				if reason == "" {
					reason = "未配置"
				}
				line += "（不可用：" + reason + "）"
			}
			lines = append(lines, line)
		}
		lines = append(lines, "用 /se <编号> 或 /se <名称> 切换")
		return modJoin(lines)
	}

	if n, err := strconv.Atoi(arg); err == nil {
		if n < 1 || n > len(engines) {
			return fmt.Sprintf("编号超出范围：1-%d", len(engines))
		}
		arg = engines[n-1].Name
	}
	if err := sw.SetSearchEngine(arg); err != nil {
		return "切换失败：" + err.Error()
	}
	return "已切到 " + sw.CurrentSearchEngine()
}
