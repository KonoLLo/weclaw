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
	timeRange string // only providers that accept a time range fill this in
	statePath string
}

type searchState struct {
	Provider  string `json:"provider"`
	TimeRange string `json:"time_range,omitempty"`
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

func loadSearchState(path string) searchState {
	b, err := os.ReadFile(path)
	if err != nil {
		return searchState{}
	}
	var st searchState
	if json.Unmarshal(b, &st) != nil {
		return searchState{}
	}
	return st
}

func saveSearchState(path string, st searchState) {
	b, err := json.MarshalIndent(st, "", " ")
	if err != nil {
		return
	}
	_ = os.MkdirAll(filepath.Dir(path), 0o700)
	_ = os.WriteFile(path, b, 0o600)
}

// timeRangeProvider is implemented by searchers that can narrow results by
// publish time (SearXNG today). Others ignore /se time.
type timeRangeProvider interface {
	SetTimeRange(string) error
	TimeRange() string
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
	case "searxng", "searx", "sx":
		return "searxng"
	case "merge", "merged", "both", "合并", "合并搜索":
		return "合并"
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
	st := loadSearchState(statePath)
	candidates := []string{st.Provider, preferred, "duckduckgo"}
	chosen := false
	for _, c := range candidates {
		if i := p.indexOf(c); i >= 0 && p.providers[i].Searcher != nil {
			p.current = i
			chosen = true
			break
		}
	}
	if !chosen {
		for i := range p.providers {
			if p.providers[i].Searcher != nil {
				p.current = i
				break
			}
		}
	}
	// Saved range wins; otherwise the searcher's own default stays.
	if st.TimeRange != "" {
		p.applyTimeRange(st.TimeRange)
	} else {
		p.timeRange = p.providerTimeRange()
	}
	return p
}

// applyTimeRange pushes v into every provider that understands it and mirrors
// the value the searcher actually kept. Returns true when at least one
// provider accepted it.
func (p *SearchPool) applyTimeRange(v string) bool {
	applied := false
	for _, pr := range p.providers {
		tr, ok := pr.Searcher.(timeRangeProvider)
		if !ok {
			continue
		}
		applied = true
		if v != "" {
			_ = tr.SetTimeRange(v)
		}
		p.timeRange = tr.TimeRange()
	}
	return applied
}

func (p *SearchPool) providerTimeRange() string {
	for _, pr := range p.providers {
		if tr, ok := pr.Searcher.(timeRangeProvider); ok {
			return tr.TimeRange()
		}
	}
	return ""
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
	st := p.snapshotLocked()
	path := p.statePath
	p.mu.Unlock()
	if path != "" {
		saveSearchState(path, st)
	}
	return nil
}

// SetTimeRange changes the publish-date window used by providers that support
// it (SearXNG) and remembers the choice for the next start.
func (p *SearchPool) SetTimeRange(v string) error {
	if _, err := NormalizeSearchTimeRange(v); err != nil {
		return err
	}
	p.mu.Lock()
	if !p.applyTimeRange(v) {
		p.mu.Unlock()
		return fmt.Errorf("当前搜索源不支持时间范围（只有 SearXNG 支持）")
	}
	st := p.snapshotLocked()
	path := p.statePath
	p.mu.Unlock()
	if path != "" {
		saveSearchState(path, st)
	}
	return nil
}

func (p *SearchPool) TimeRange() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.timeRange
}

// snapshotLocked copies what must survive a restart. An empty range is stored
// as "off" so the next start can tell it apart from "never chosen".
func (p *SearchPool) snapshotLocked() searchState {
	name := ""
	if p.current >= 0 && p.current < len(p.providers) {
		name = strings.ToLower(strings.TrimSpace(p.providers[p.current].Name))
	}
	tr := p.timeRange
	if tr == "" {
		tr = "off"
	}
	return searchState{Provider: name, TimeRange: tr}
}
