package cmd

import (
	"testing"

	"github.com/fastclaw-ai/weclaw/agent"
	"github.com/fastclaw-ai/weclaw/config"
)

func TestNewWebSearchRegistersSearXNG(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Search = config.SearchConfig{
		SearXNGURL: "https://search.example.com",
		Provider:   "searxng",
	}
	pool, ok := newWebSearch(cfg).(*agent.SearchPool)
	if !ok {
		t.Fatal("newWebSearch should return a SearchPool")
	}
	infos := pool.SearchEngines()
	want := []string{"DuckDuckGo", "Tavily", "Brave", "SearXNG", "合并"}
	if len(infos) != len(want) {
		t.Fatalf("providers = %d, want %d: %+v", len(infos), len(want), infos)
	}
	for i, name := range want {
		if infos[i].Name != name {
			t.Errorf("provider %d = %q, want %q", i, infos[i].Name, name)
		}
	}
	if !infos[3].Available {
		t.Fatalf("SearXNG should be available with a URL: %+v", infos)
	}
	if !infos[4].Available {
		t.Fatalf("合并 should be available when SearXNG is: %+v", infos)
	}
	if pool.Name() != "SearXNG" {
		t.Fatalf("current = %q, want SearXNG", pool.Name())
	}
}

func TestNewWebSearchMergeSelection(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Search = config.SearchConfig{
		SearXNGURL: "https://search.example.com",
		Provider:   "合并",
	}
	pool := newWebSearch(cfg).(*agent.SearchPool)
	if pool.Name() != "合并" {
		t.Fatalf("provider=合并 should select it, got %q", pool.Name())
	}
}

func TestNewWebSearchExplainsMissingSearXNGURL(t *testing.T) {
	t.Setenv("SEARXNG_URL", "")
	cfg := config.DefaultConfig()
	pool := newWebSearch(cfg).(*agent.SearchPool)
	infos := pool.SearchEngines()
	if infos[3].Available || infos[3].Detail != "没配 searxng_url" {
		t.Fatalf("searxng entry = %+v", infos[3])
	}
	if pool.Name() != "DuckDuckGo" {
		t.Fatalf("default = %q, want DuckDuckGo", pool.Name())
	}
}
