package agent

import (
	"context"
	"path/filepath"
	"testing"
)

type fakeSearcher struct {
	name string
	got  string
}

func (f *fakeSearcher) Name() string { return f.name }

func (f *fakeSearcher) Search(ctx context.Context, query string) (string, []SearchResult, error) {
	f.got = query
	return "answer", []SearchResult{{Title: "t", URL: "u", Content: "c"}}, nil
}

func TestSearchKeyAliases(t *testing.T) {
	cases := map[string]string{
		"ddg": "duckduckgo", "DuckDuckGo": "duckduckgo",
		"tavily": "tavily",
		"br":     "brave", "Brave": "brave",
		"searx": "searxng", "SearXNG": "searxng",
	}
	for in, want := range cases {
		if got := searchKey(in); got != want {
			t.Fatalf("searchKey(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSearchPoolSwitchAndPersist(t *testing.T) {
	state := filepath.Join(t.TempDir(), "search.json")
	ddg := &fakeSearcher{name: "DuckDuckGo"}
	tavily := &fakeSearcher{name: "Tavily"}
	providers := []SearchProvider{
		{Name: "DuckDuckGo", Searcher: ddg},
		{Name: "Tavily", Searcher: tavily},
	}

	p := newSearchPoolAt(providers, "", state)
	if p.Name() != "DuckDuckGo" {
		t.Fatalf("default = %q, want DuckDuckGo", p.Name())
	}
	if err := p.SetSearchEngine("ddg"); err != nil {
		t.Fatalf("switch to alias ddg: %v", err)
	}
	if err := p.SetSearchEngine("tavily"); err != nil {
		t.Fatalf("switch to tavily: %v", err)
	}
	if p.Name() != "Tavily" {
		t.Fatalf("current = %q, want Tavily", p.Name())
	}
	if _, _, err := p.Search(context.Background(), "机票"); err != nil {
		t.Fatalf("search: %v", err)
	}
	if tavily.got != "机票" {
		t.Fatalf("delegated query = %q, want 机票", tavily.got)
	}

	// A fresh process must pick up the saved choice over the config default.
	p2 := newSearchPoolAt(providers, "duckduckgo", state)
	if p2.Name() != "Tavily" {
		t.Fatalf("reloaded current = %q, want Tavily", p2.Name())
	}
}

type fakeTimeSearcher struct {
	fakeSearcher
	tr string // empty means the provider itself defaults to none
}

func (f *fakeTimeSearcher) SetTimeRange(v string) error {
	tr, err := NormalizeSearchTimeRange(v)
	if err != nil {
		return err
	}
	f.tr = tr
	return nil
}

func (f *fakeTimeSearcher) TimeRange() string { return f.tr }

func TestSearchPoolTimeRangePersists(t *testing.T) {
	state := filepath.Join(t.TempDir(), "search.json")
	newProviders := func() []SearchProvider {
		return []SearchProvider{
			{Name: "DuckDuckGo", Searcher: &fakeSearcher{name: "DuckDuckGo"}},
			{Name: "SearXNG", Searcher: &fakeTimeSearcher{fakeSearcher: fakeSearcher{name: "SearXNG"}, tr: "week"}},
		}
	}

	p := newSearchPoolAt(newProviders(), "searxng", state)
	if p.TimeRange() != "week" {
		t.Fatalf("default = %q, want week", p.TimeRange())
	}
	if err := p.SetTimeRange("day"); err != nil {
		t.Fatalf("set day: %v", err)
	}

	// Switching providers must not forget the time range.
	if err := p.SetSearchEngine("duckduckgo"); err != nil {
		t.Fatal(err)
	}
	p2 := newSearchPoolAt(newProviders(), "", state)
	if p2.Name() != "DuckDuckGo" || p2.TimeRange() != "day" {
		t.Fatalf("reloaded = (%q, %q), want (DuckDuckGo, day)", p2.Name(), p2.TimeRange())
	}

	// "off" must survive as off, not fall back to the provider default.
	if err := p2.SetTimeRange("off"); err != nil {
		t.Fatal(err)
	}
	p3 := newSearchPoolAt(newProviders(), "", state)
	if p3.TimeRange() != "" {
		t.Fatalf("reloaded off = %q, want empty", p3.TimeRange())
	}
}

func TestSearchPoolTimeRangeUnsupported(t *testing.T) {
	providers := []SearchProvider{{Name: "DuckDuckGo", Searcher: &fakeSearcher{name: "DuckDuckGo"}}}
	p := newSearchPoolAt(providers, "", filepath.Join(t.TempDir(), "search.json"))
	if err := p.SetTimeRange("day"); err == nil {
		t.Fatal("want unsupported error when no provider takes a time range")
	}
	if p.TimeRange() != "" {
		t.Fatalf("state changed on failure: %q", p.TimeRange())
	}
}

func TestSearchPoolUnavailable(t *testing.T) {
	providers := []SearchProvider{
		{Name: "DuckDuckGo", Searcher: &fakeSearcher{name: "DuckDuckGo"}},
		{Name: "Tavily", Unavailable: "没配 api_key"},
	}
	p := newSearchPoolAt(providers, "tavily", filepath.Join(t.TempDir(), "search.json"))
	if p.Name() != "DuckDuckGo" {
		t.Fatalf("unavailable preferred should fall back, got %q", p.Name())
	}
	if err := p.SetSearchEngine("2"); err == nil {
		t.Fatal("switching by number should not resolve, want unknown name error")
	}
	if err := p.SetSearchEngine("Tavily"); err == nil {
		t.Fatal("want unavailable error")
	}
	infos := p.SearchEngines()
	if len(infos) != 2 || infos[1].Available || !infos[0].Current {
		t.Fatalf("unexpected engine table: %+v", infos)
	}
}
