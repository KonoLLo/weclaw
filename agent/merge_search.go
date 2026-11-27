package agent

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
)

// MergeSearch runs two backends for one query and interleaves what comes back.
// It backs the /se "合并" entry: SearXNG answers from the self-hosted instance
// while WeClaw's DuckDuckGo client uses a different upstream endpoint, so the
// two cover each other's gaps (blocked engines, captcha, region).
//
// A time filter only exists on SearXNG and DuckDuckGo cannot honour it, so
// when the filter is on MergeSearch asks SearXNG alone rather than mixing in
// older pages.
type MergeSearch struct {
	primary    *SearXNG
	secondary  Searcher
	maxResults int
}

func NewMergeSearch(primary *SearXNG, secondary Searcher, maxResults int) *MergeSearch {
	if maxResults <= 0 || maxResults > 20 {
		maxResults = DefaultSearchMaxResult
	}
	return &MergeSearch{primary: primary, secondary: secondary, maxResults: maxResults}
}

func (s *MergeSearch) Name() string { return "合并" }

func (s *MergeSearch) SetTimeRange(v string) error { return s.primary.SetTimeRange(v) }

func (s *MergeSearch) TimeRange() string { return s.primary.TimeRange() }

type mergeOutcome struct {
	fromPrimary bool
	answer      string
	results     []SearchResult
	err         error
}

func (s *MergeSearch) Search(ctx context.Context, query string) (string, []SearchResult, error) {
	if s.primary == nil {
		return s.secondary.Search(ctx, query)
	}
	if s.primary.TimeRange() != "" || s.secondary == nil {
		return s.primary.Search(ctx, query)
	}

	var wg sync.WaitGroup
	ch := make(chan mergeOutcome, 2)
	run := func(fromPrimary bool, s Searcher) {
		defer wg.Done()
		answer, results, err := s.Search(ctx, query)
		ch <- mergeOutcome{fromPrimary: fromPrimary, answer: answer, results: results, err: err}
	}
	wg.Add(2)
	go run(true, s.primary)
	go run(false, s.secondary)
	wg.Wait()
	close(ch)

	var primary, secondary mergeOutcome
	for out := range ch {
		if out.fromPrimary {
			primary = out
		} else {
			secondary = out
		}
	}

	switch {
	case primary.err != nil && secondary.err != nil:
		return "", nil, fmt.Errorf("两路搜索都没成功：SearXNG：%v；DuckDuckGo：%v", primary.err, secondary.err)
	case primary.err != nil:
		log.Printf("[search] merge: SearXNG failed, using DuckDuckGo only: %v", primary.err)
		return secondary.answer, secondary.results, nil
	case secondary.err != nil:
		log.Printf("[search] merge: DuckDuckGo failed, using SearXNG only: %v", secondary.err)
		return primary.answer, primary.results, nil
	}

	answer := strings.TrimSpace(primary.answer)
	if extra := strings.TrimSpace(secondary.answer); extra != "" {
		if answer == "" {
			answer = extra
		} else if !strings.Contains(answer, extra) {
			answer += "\n" + extra
		}
	}
	limit := s.maxResults * 2
	if limit < 10 {
		limit = 10
	}
	return answer, interleaveResults(primary.results, secondary.results, limit), nil
}

// interleaveResults alternates the two lists so neither backend monopolises
// the top slots. Duplicates keep the primary (SearXNG) copy, so a secondary
// hit is dropped when the primary list has the same URL.
func interleaveResults(a, b []SearchResult, limit int) []SearchResult {
	if limit <= 0 {
		limit = 10
	}
	out := make([]SearchResult, 0, minInt(limit, len(a)+len(b)))
	seen := make(map[string]bool, len(a)+len(b))
	primaryURLs := make(map[string]bool, len(a))
	for _, r := range a {
		if key := normalizeResultURL(r.URL); key != "" {
			primaryURLs[key] = true
		}
	}
	add := func(r SearchResult, fromPrimary bool) bool {
		key := normalizeResultURL(r.URL)
		if key == "" || seen[key] {
			return false
		}
		if !fromPrimary && primaryURLs[key] {
			return false // primary's copy will be appended in its turn
		}
		seen[key] = true
		r.URL = key
		out = append(out, r)
		return len(out) >= limit
	}
	for i := 0; i < len(a) || i < len(b); i++ {
		if i < len(a) && add(a[i], true) {
			return out
		}
		if i < len(b) && add(b[i], false) {
			return out
		}
	}
	return out
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
