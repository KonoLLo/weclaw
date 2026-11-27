package messaging

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/fastclaw-ai/weclaw/agent"
)

type fakeSearchSwitcher struct {
	engines []agent.SearchEngineInfo
	current int
}

func (f *fakeSearchSwitcher) Name() string { return f.engines[f.current].Name }

func (f *fakeSearchSwitcher) Search(ctx context.Context, query string) (string, []agent.SearchResult, error) {
	return "", nil, nil
}

func (f *fakeSearchSwitcher) SearchEngines() []agent.SearchEngineInfo {
	out := make([]agent.SearchEngineInfo, len(f.engines))
	for i, e := range f.engines {
		e.Current = i == f.current
		out[i] = e
	}
	return out
}

func (f *fakeSearchSwitcher) CurrentSearchEngine() string { return f.Name() }

func (f *fakeSearchSwitcher) SetSearchEngine(name string) error {
	for i, e := range f.engines {
		if strings.EqualFold(e.Name, name) {
			if !e.Available {
				return fmt.Errorf("%s 不可用", e.Name)
			}
			f.current = i
			return nil
		}
	}
	return fmt.Errorf("没有这个搜索源：%s", name)
}

func newFakeSearchHandler() *Handler {
	return &Handler{webSearch: &fakeSearchSwitcher{engines: []agent.SearchEngineInfo{
		{Name: "DuckDuckGo", Available: true},
		{Name: "Tavily", Available: false, Detail: "没配 api_key"},
	}}}
}

type fakeTimeSwitcher struct {
	fakeSearchSwitcher
	tr string
}

func (f *fakeTimeSwitcher) SetTimeRange(v string) error {
	tr, err := agent.NormalizeSearchTimeRange(v)
	if err != nil {
		return err
	}
	f.tr = tr
	return nil
}

func (f *fakeTimeSwitcher) TimeRange() string { return f.tr }

func TestParseSearchEngineArgs(t *testing.T) {
	cases := []struct {
		text string
		arg  string
		ok   bool
	}{
		{"/se", "", true},
		{"/se 2", "2", true},
		{"/SE tavily", "tavily", true},
		{"/sengine 1", "1", true},
		{"/搜源 2", "2", true},
		{"/s 机票", "", false},
		{"/sefoo", "", false},
	}
	for _, c := range cases {
		arg, ok := parseSearchEngineArgs(c.text)
		if arg != c.arg || ok != c.ok {
			t.Errorf("parse(%q) = (%q,%v), want (%q,%v)", c.text, arg, ok, c.arg, c.ok)
		}
	}
}

func TestSearchTimeRangeCommand(t *testing.T) {
	h := newFakeSearchHandler()
	if got := h.handleSearchEngineCommand("/se time day"); !strings.Contains(got, "不支持") {
		t.Errorf("plain switcher should reject /se time, got %q", got)
	}

	f := &fakeTimeSwitcher{fakeSearchSwitcher: fakeSearchSwitcher{engines: []agent.SearchEngineInfo{
		{Name: "DuckDuckGo", Available: true},
		{Name: "SearXNG", Available: true},
	}}, tr: "week"}
	h.webSearch = f

	if got := h.handleSearchEngineCommand("/se time"); !strings.Contains(got, "最近 7 天") {
		t.Errorf("report = %q", got)
	}
	if got := h.handleSearchEngineCommand("/se time day"); got != "时间范围已设为最近 24 小时（只对 SearXNG 生效，Brave、Bing 不参与）" {
		t.Errorf("set day = %q", got)
	}
	if f.TimeRange() != "day" {
		t.Errorf("state = %q, want day", f.TimeRange())
	}
	if got := h.handleSearchEngineCommand("/se 时间 day"); got != "时间范围已设为最近 24 小时（只对 SearXNG 生效，Brave、Bing 不参与）" {
		t.Errorf("Chinese alias = %q", got)
	}
	if got := h.handleSearchEngineCommand("/se time off"); got != "时间范围已关闭" {
		t.Errorf("off = %q", got)
	}
	if got := h.handleSearchEngineCommand("/se time 去年"); !strings.Contains(got, "设置失败") {
		t.Errorf("invalid = %q", got)
	}
	if list := h.handleSearchEngineCommand("/se list"); !strings.Contains(list, "搜索结果时间：不限时间") {
		t.Errorf("list should report time range:\n%s", list)
	}
	// /se off must still mean auto-search off, not time range.
	if got := h.handleSearchEngineCommand("/se off"); got != "智能搜索已关闭" {
		t.Errorf("/se off = %q", got)
	}
}

func TestSearchEngineListAndSwitch(t *testing.T) {
	h := newFakeSearchHandler()

	list := h.handleSearchEngineCommand("/se")
	for _, want := range []string{"1) DuckDuckGo  ←当前", "2) Tavily（不可用：没配 api_key）", "/se <编号>"} {
		if !strings.Contains(list, want) {
			t.Errorf("list missing %q:\n%s", want, list)
		}
	}

	if got := h.handleSearchEngineCommand("/se list"); !strings.Contains(got, "←当前") {
		t.Errorf("list word = %q", got)
	}
	if got := h.handleSearchEngineCommand("/se 2"); !strings.Contains(got, "切换失败") || !strings.Contains(got, "不可用") {
		t.Errorf("switch to unavailable = %q", got)
	}
	if got := h.handleSearchEngineCommand("/se 9"); !strings.Contains(got, "编号超出范围") {
		t.Errorf("out of range = %q", got)
	}
	if got := h.handleSearchEngineCommand("/se duckduckgo"); got != "已切到 DuckDuckGo" {
		t.Errorf("switch by name = %q", got)
	}
}

func TestAutoSearchOnOffRepliesAndPersists(t *testing.T) {
	h := newFakeSearchHandler()
	var saved []bool
	h.SetAutoSearchSaver(func(on bool) error {
		saved = append(saved, on)
		return nil
	})

	if got := h.handleSearchEngineCommand("/se on"); got != "智能搜索已开启" {
		t.Fatalf("/se on reply = %q, want 智能搜索已开启", got)
	}
	if !h.autoSearchEnabled() {
		t.Fatal("auto search should be enabled after /se on")
	}
	if got := h.handleSearchEngineCommand("/se off"); got != "智能搜索已关闭" {
		t.Fatalf("/se off reply = %q, want 智能搜索已关闭", got)
	}
	if h.autoSearchEnabled() {
		t.Fatal("auto search should be disabled after /se off")
	}
	if len(saved) != 2 || !saved[0] || saved[1] {
		t.Fatalf("persisted states = %v, want [true false]", saved)
	}

	list := h.handleSearchEngineCommand("/se")
	if !strings.Contains(list, "智能搜索（回复前自动联网）：已关闭") {
		t.Errorf("list should show auto search state:\n%s", list)
	}
	if got := h.handleSearchEngineCommand("/se 开启"); got != "智能搜索已开启" {
		t.Errorf("Chinese keyword /se 开启 = %q", got)
	}
}

func TestAutoSearchSaveFailureStillSwitches(t *testing.T) {
	h := newFakeSearchHandler()
	h.SetAutoSearchSaver(func(bool) error { return fmt.Errorf("disk full") })

	got := h.handleSearchEngineCommand("/se on")
	if !strings.Contains(got, "智能搜索已开启") || !strings.Contains(got, "保存配置失败") {
		t.Fatalf("reply = %q, want switched state plus save warning", got)
	}
	if !h.autoSearchEnabled() {
		t.Fatal("runtime state should still switch when saving fails")
	}
}
