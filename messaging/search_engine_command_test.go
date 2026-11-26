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
