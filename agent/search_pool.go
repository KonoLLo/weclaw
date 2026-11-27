package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// SearchProvider is one entry in the /se table. A nil Searcher means the
// provider is known but not configured; Unavailable explains why.
type SearchProvider struct {
	Name        string
	Searcher    Searcher
	Unavailable string
}

// SearchEngineInfo is the read-only view /se prints.
type SearchEngineInfo struct {
	Name      string
	Available bool
	Detail    string
	Current   bool
}

// SearchPool makes the /s backend runtime-switchable and remembers the choice
// across restarts. It implements Searcher so Handler keeps one field.
type SearchPool struct {
	mu        sync.RWMutex
	providers []SearchProvider
	current   int
	statePath string
}

// searchStatePath mirrors modelStatePath: the selection is user state, not
// config, so switching does not rewrite config.json.
func searchStatePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".weclaw", "search.json"), nil
}

func loadSearchSelection(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var s struct {
		Provider string `json:"provider"`
	}
	if json.Unmarshal(b, &s) != nil {
		return ""
	}
	return s.Provider
}

func saveSearchSelection(path, name string) {
	b, err := json.MarshalIndent(map[string]string{"provider": strings.ToLower(strings.TrimSpace(name))}, "", " ")
	if err != nil {
		return
	}
	_ = os.MkdirAll(filepath.Dir(path), 0o700)
	_ = os.WriteFile(path, b, 0o600)
}

func searchKey(name string) string {
	k := strings.ToLower(strings.TrimSpace(name))
	switch k {
	case "ddg", "duck", "duckduckgo":
		return "duckduckgo"
	case "tavily":
		return "tavily"
	case "brave", "br":
		return "brave"
	}
	return k
}

// NewSearchPool builds the table, then picks: remembered choice, then
// preferred (config), then the first usable provider.
func NewSearchPool(providers []SearchProvider, preferred string) *SearchPool {
	path, _ := searchStatePath()
	return newSearchPoolAt(providers, preferred, path)
}

func newSearchPoolAt(providers []SearchProvider, preferred, statePath string) *SearchPool {
	p := &SearchPool{providers: providers, statePath: statePath}
	candidates := []string{}
	if statePath != "" {
		candidates = append(candidates, loadSearchSelection(statePath))
	}
	candidates = append(candidates, preferred, "duckduckgo")
	for _, c := range candidates {
		if i := p.indexOf(c); i >= 0 && p.providers[i].Searcher != nil {
			p.current = i
			return p
		}
	}
	for i := range p.providers {
		if p.providers[i].Searcher != nil {
			p.current = i
			return p
		}
	}
	return p
}

func (p *SearchPool) indexOf(name string) int {
	k := searchKey(name)
	if k == "" {
		return -1
	}
	for i, pr := range p.providers {
		if searchKey(pr.Name) == k {
			return i
		}
	}
	return -1
}

func (p *SearchPool) Name() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.current < 0 || p.current >= len(p.providers) {
		return ""
	}
	return p.providers[p.current].Name
}

func (p *SearchPool) Search(ctx context.Context, query string) (string, []SearchResult, error) {
	p.mu.RLock()
	var s Searcher
	if p.current >= 0 && p.current < len(p.providers) {
		s = p.providers[p.current].Searcher
	}
	p.mu.RUnlock()
	if s == nil {
		return "", nil, fmt.Errorf("没有可用的搜索源")
	}
	return s.Search(ctx, query)
}

func (p *SearchPool) SearchEngines() []SearchEngineInfo {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]SearchEngineInfo, len(p.providers))
	for i, pr := range p.providers {
		out[i] = SearchEngineInfo{
			Name:      pr.Name,
			Available: pr.Searcher != nil,
			Detail:    pr.Unavailable,
			Current:   i == p.current,
		}
	}
	return out
}

func (p *SearchPool) CurrentSearchEngine() string { return p.Name() }

func (p *SearchPool) SetSearchEngine(name string) error {
	p.mu.Lock()
	i := p.indexOf(name)
	if i < 0 {
		p.mu.Unlock()
		return fmt.Errorf("没有这个搜索源：%s", name)
	}
	if p.providers[i].Searcher == nil {
		reason := p.providers[i].Unavailable
		if reason == "" {
			reason = "未配置"
		}
		display := p.providers[i].Name
		p.mu.Unlock()
		return fmt.Errorf("%s 不可用：%s", display, reason)
	}
	p.current = i
	display := p.providers[i].Name
	path := p.statePath
	p.mu.Unlock()
	if path != "" {
		saveSearchSelection(path, display)
	}
	return nil
}
