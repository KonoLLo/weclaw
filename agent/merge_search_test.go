package agent

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func newSearXNGTestServer(t *testing.T, body string) *SearXNG {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	return NewSearXNG(server.URL, "", 5)
}

type countingSearcher struct {
	results []SearchResult
	err     error
	calls   int
}

func (c *countingSearcher) Name() string { return "DuckDuckGo" }

func (c *countingSearcher) Search(ctx context.Context, query string) (string, []SearchResult, error) {
	c.calls++
	return "", c.results, c.err
}

func TestMergeSearchInterleavesAndDedupes(t *testing.T) {
	primary := newSearXNGTestServer(t, `{"results":[
		{"title":"a1","url":"https://example.com/1"},
		{"title":"a2","url":"https://example.com/2/"},
		{"title":"dup","url":"https://example.com/3"}
	]}`)
	secondary := &countingSearcher{results: []SearchResult{
		{Title: "b1", URL: "https://other.com/1"},
		{Title: "b-dup", URL: "https://example.com/3"},
		{Title: "b2", URL: "https://other.com/2"},
	}}

	m := NewMergeSearch(primary, secondary, 5)
	_, results, err := m.Search(context.Background(), "q")
	if err != nil {
		t.Fatal(err)
	}
	var titles []string
	for _, r := range results {
		titles = append(titles, r.Title)
	}
	want := []string{"a1", "b1", "a2", "dup", "b2"}
	if len(titles) != len(want) {
		t.Fatalf("titles = %v, want %v", titles, want)
	}
	for i := range want {
		if titles[i] != want[i] {
			t.Fatalf("titles = %v, want %v", titles, want)
		}
	}
	if secondary.calls != 1 {
		t.Fatalf("secondary calls = %d", secondary.calls)
	}
}

func TestMergeSearchTimeRangeSkipsSecondary(t *testing.T) {
	primary := newSearXNGTestServer(t, `{"results":[{"title":"a1","url":"https://example.com/1"}]}`)
	secondary := &countingSearcher{results: []SearchResult{{Title: "b1", URL: "https://other.com/1"}}}
	m := NewMergeSearch(primary, secondary, 5)
	if err := m.SetTimeRange("week"); err != nil {
		t.Fatal(err)
	}
	if m.TimeRange() != "week" {
		t.Fatalf("TimeRange() = %q", m.TimeRange())
	}
	_, results, err := m.Search(context.Background(), "q")
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Title != "a1" {
		t.Fatalf("results = %+v, want SearXNG only", results)
	}
	if secondary.calls != 0 {
		t.Fatalf("DuckDuckGo should be skipped when a time range is set, calls=%d", secondary.calls)
	}
}

func TestMergeSearchFallsBackWhenOneSideFails(t *testing.T) {
	primary := newSearXNGTestServer(t, `{"results":[{"title":"a1","url":"https://example.com/1"}]}`)
	broken := &countingSearcher{err: errors.New("boom")}
	m := NewMergeSearch(primary, broken, 5)
	_, results, err := m.Search(context.Background(), "q")
	if err != nil || len(results) != 1 {
		t.Fatalf("want SearXNG fallback, got results=%d err=%v", len(results), err)
	}

	badPrimary := NewSearXNG("http://127.0.0.1:1", "", 5)
	good := &countingSearcher{results: []SearchResult{{Title: "b1", URL: "https://other.com/1"}}}
	m2 := NewMergeSearch(badPrimary, good, 5)
	_, results, err = m2.Search(context.Background(), "q")
	if err != nil || len(results) != 1 || results[0].Title != "b1" {
		t.Fatalf("want DuckDuckGo fallback, got results=%+v err=%v", results, err)
	}
}
