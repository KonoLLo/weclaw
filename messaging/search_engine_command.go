package messaging

import (
	"fmt"
	"log"
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

// searchTimeRanger is optional: only SearXNG narrows results by publish date,
// so /se time answers "不支持" for the other backends.
type searchTimeRanger interface {
	SetTimeRange(string) error
	TimeRange() string
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
		return "用法：/se 查看，/se <编号> 切换，/se on|off 开关智能搜索，/se time 限制时间范围"
	}
	switch strings.ToLower(arg) {
	case "on", "开", "开启":
		return h.switchAutoSearch(true)
	case "off", "关", "关闭":
		return h.switchAutoSearch(false)
	}
	if v, ok := splitTimeRangeArg(arg); ok {
		return h.switchSearchTimeRange(v)
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
		lines = append(lines, "智能搜索（回复前自动联网）："+h.autoSearchLabel()+"，用 /se on 或 /se off 切换")
		if tr, ok := h.webSearch.(searchTimeRanger); ok {
			lines = append(lines, "搜索结果时间："+searchTimeRangeLabel(tr.TimeRange())+"，用 /se time day|week|month|year|off 切换（仅 SearXNG）")
		}
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

// switchAutoSearch flips the reply-time web search and reports the new state.
func (h *Handler) switchAutoSearch(on bool) string {
	h.SetAutoSearch(on)
	state := h.autoSearchLabel()
	if h.saveAutoSearch != nil {
		if err := h.saveAutoSearch(on); err != nil {
			log.Printf("[search] failed to save auto-search state: %v", err)
			return "智能搜索" + state + "（保存配置失败，重启后可能恢复原状态）"
		}
	}
	return "智能搜索" + state
}

func (h *Handler) autoSearchLabel() string {
	if h.autoSearchEnabled() {
		return "已开启"
	}
	return "已关闭"
}

// splitTimeRangeArg matches "/se time week" and "/se 时间 off".
func splitTimeRangeArg(arg string) (string, bool) {
	fields := strings.Fields(arg)
	if len(fields) == 0 {
		return "", false
	}
	switch strings.ToLower(fields[0]) {
	case "time", "tr", "时间", "时间范围":
		return strings.Join(fields[1:], " "), true
	}
	return "", false
}

// switchSearchTimeRange sets (or reports) the publish-date window. Only
// SearXNG honours it; SearXNG drops engines that cannot filter, so the reply
// warns that Brave and Bing drop out of the results.
func (h *Handler) switchSearchTimeRange(v string) string {
	tr, ok := h.webSearch.(searchTimeRanger)
	if !ok {
		return "当前搜索源不支持时间范围（只有 SearXNG 支持）"
	}
	if strings.TrimSpace(v) == "" {
		return "当前时间范围：" + searchTimeRangeLabel(tr.TimeRange()) + "\n用 /se time day|week|month|year|off 切换"
	}
	if err := tr.SetTimeRange(v); err != nil {
		return "设置失败：" + err.Error()
	}
	cur := tr.TimeRange()
	if cur == "" {
		return "时间范围已关闭"
	}
	return "时间范围已设为" + searchTimeRangeLabel(cur) + "（只对 SearXNG 生效，Brave、Bing 不参与）"
}

func searchTimeRangeLabel(v string) string {
	switch v {
	case "day":
		return "最近 24 小时"
	case "week":
		return "最近 7 天"
	case "month":
		return "最近 30 天"
	case "year":
		return "最近 1 年"
	}
	return "不限时间"
}
