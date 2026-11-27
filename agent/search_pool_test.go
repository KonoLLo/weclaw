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
