package agent

import (
	"context"
	"fmt"
	"log"
	"net/url"
	"strings"
	"sync"
)

const (
	DeepSearchMaxQueries   = 3
	DeepSearchDefaultPages = 6
	deepSearchFetchWorkers = 6
)

// DeepSearchBundle is everything one deep-search round gathered: the merged
// results across queries plus the readable text of the top pages.
type DeepSearchBundle struct {
	Queries []string
	Answer  string
	Results []SearchResult
	Pages   map[string]string // normalized URL -> article text
}

// DeepSearch runs up to three unique queries, merges results by URL and
// fetches the top pages concurrently. It only fails when every query failed.
func DeepSearch(ctx context.Context, s Searcher, queries []string, maxPages int) (DeepSearchBundle, error) {
	var bundle DeepSearchBundle
	if s == nil {
		return bundle, fmt.Errorf("没有可用的搜索引擎")
	}
	if maxPages <= 0 {
		maxPages = DeepSearchDefaultPages
	}
	seenQuery := make(map[string]bool)
	for _, q := range queries {
		q = strings.TrimSpace(q)
		if q == "" || seenQuery[strings.ToLower(q)] {
			continue
		}
		seenQuery[strings.ToLower(q)] = true
		bundle.Queries = append(bundle.Queries, q)
		if len(bundle.Queries) >= DeepSearchMaxQueries {
			break
		}
	}
	if len(bundle.Queries) == 0 {
		return bundle, fmt.Errorf("没有可用的检索词")
	}

	var firstErr error
	searchOK := 0
	seenURL := make(map[string]bool)
	for _, q := range bundle.Queries {
		answer, results, err := s.Search(ctx, q)
		if err != nil {
			log.Printf("[search] query %q failed: %v", q, err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		searchOK++
		if bundle.Answer == "" && strings.TrimSpace(answer) != "" {
			bundle.Answer = strings.TrimSpace(answer)
		}
		for _, r := range results {
			u := normalizeResultURL(r.URL)
			if u == "" || seenURL[u] {
				continue
			}
			seenURL[u] = true
			r.URL = u
			bundle.Results = append(bundle.Results, r)
		}
	}
	if searchOK == 0 {
		return bundle, fmt.Errorf("所有检索词都没搜成：%w", firstErr)
	}
	bundle.Pages = fetchTopPages(ctx, bundle.Results, maxPages)
	return bundle, nil
}

func fetchTopPages(ctx context.Context, results []SearchResult, max int) map[string]string {
	pages := make(map[string]string)
	pending := make([]string, 0, max)
	for _, r := range results {
		u := normalizeResultURL(r.URL)
		if u == "" {
			continue
		}
		// Providers like Tavily crawl the page themselves; use that text
		// instead of fetching (those fetches are what sites block).
		if text := strings.TrimSpace(r.RawContent); text != "" {
			pages[u] = truncateText(text, pageTextMaxRunes)
			continue
		}
		if len(pending) >= max {
			continue
		}
		pending = append(pending, u)
	}
	if len(pending) == 0 {
		return pages
	}
	sem := make(chan struct{}, deepSearchFetchWorkers)
	var wg sync.WaitGroup
	var mu sync.Mutex
	for _, u := range pending {
		wg.Add(1)
		go func(pageURL string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			text, err := PageTextFetcher(ctx, pageURL, pageTextMaxRunes)
			if err != nil {
				log.Printf("[search] page %s skipped: %v", hostOf(pageURL), err)
				return
			}
			mu.Lock()
			pages[pageURL] = text
			mu.Unlock()
		}(u)
	}
	wg.Wait()
	return pages
}

// normalizeResultURL unwraps DuckDuckGo's /l/?uddg= redirector, fixes
// protocol-relative links and drops anything that is not plain http(s).
func normalizeResultURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if strings.HasPrefix(raw, "//") {
		raw = "https:" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return ""
	}
	if strings.HasSuffix(strings.ToLower(u.Hostname()), "duckduckgo.com") && strings.HasPrefix(u.Path, "/l/") {
		if target := u.Query().Get("uddg"); target != "" {
			if tu, err := url.Parse(target); err == nil && (tu.Scheme == "http" || tu.Scheme == "https") {
				u = tu
			}
		}
	}
	u.Fragment = ""
	return u.String()
}

func hostOf(rawURL string) string {
	if u, err := url.Parse(rawURL); err == nil && u.Hostname() != "" {
		return u.Hostname()
	}
	return rawURL
}
