package agent

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"
)

type stubSearcher struct {
	byQuery map[string][]SearchResult
	answers map[string]string
	errs    map[string]error
	queries []string
}

func (s *stubSearcher) Name() string { return "stub" }

func (s *stubSearcher) Search(_ context.Context, q string) (string, []SearchResult, error) {
	s.queries = append(s.queries, q)
	if err := s.errs[q]; err != nil {
		return "", nil, err
	}
	return s.answers[q], s.byQuery[q], nil
}

func TestNormalizeResultURL(t *testing.T) {
	cases := map[string]string{
		"https://a.example/x#top": "https://a.example/x",
		"//b.example/y":           "https://b.example/y",
		"//duckduckgo.com/l/?uddg=" + url.QueryEscape("https://c.example/z"): "https://c.example/z",
		"javascript:alert(1)": "",
		"ftp://c.example/z":   "",
		"":                    "",
	}
	for in, want := range cases {
		if got := normalizeResultURL(in); got != want {
			t.Fatalf("normalizeResultURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDeepSearchMergesDedupsAndFetches(t *testing.T) {
	old := PageTextFetcher
	PageTextFetcher = func(_ context.Context, u string, _ int) (string, error) {
		return "正文：" + u, nil
	}
	defer func() { PageTextFetcher = old }()

	uddg := "//duckduckgo.com/l/?uddg=" + url.QueryEscape("https://c.example/3")
	s := &stubSearcher{byQuery: map[string][]SearchResult{
		"甲": {{Title: "A", URL: "https://a.example/1#frag"}, {Title: "B", URL: "https://b.example/2"}},
		"乙": {{Title: "A again", URL: "https://a.example/1"}, {Title: "C", URL: uddg}},
	}}
	b, err := DeepSearch(context.Background(), s, []string{"甲", "乙", "甲"}, 3)
	if err != nil {
		t.Fatalf("deep search: %v", err)
	}
	if len(b.Queries) != 2 {
		t.Fatalf("queries = %#v, want two unique", b.Queries)
	}
	if len(b.Results) != 3 {
		t.Fatalf("results = %#v, want three unique", b.Results)
	}
	if b.Results[0].URL != "https://a.example/1" {
		t.Fatalf("fragment not stripped: %q", b.Results[0].URL)
	}
	if b.Results[2].URL != "https://c.example/3" {
		t.Fatalf("uddg not unwrapped: %q", b.Results[2].URL)
	}
	if len(b.Pages) != 3 || b.Pages["https://c.example/3"] == "" {
		t.Fatalf("pages = %#v, want three fetched", b.Pages)
	}
}

func TestDeepSearchToleratesOneFailedQuery(t *testing.T) {
	old := PageTextFetcher
	PageTextFetcher = func(context.Context, string, int) (string, error) {
		return "", errors.New("nope")
	}
	defer func() { PageTextFetcher = old }()

	s := &stubSearcher{
		byQuery: map[string][]SearchResult{"甲": {{Title: "A", URL: "https://a.example/1"}}},
		errs:    map[string]error{"乙": errors.New("blocked")},
	}
	if _, err := DeepSearch(context.Background(), s, []string{"甲", "乙"}, 2); err != nil {
		t.Fatalf("one failed query must not fail the round: %v", err)
	}
}

func TestDeepSearchAllQueriesFailed(t *testing.T) {
	s := &stubSearcher{errs: map[string]error{"甲": errors.New("boom"), "乙": errors.New("boom")}}
	if _, err := DeepSearch(context.Background(), s, []string{"甲", "乙"}, 2); err == nil {
		t.Fatal("want error when every query fails")
	}
}

func TestDeepSearchCapsPages(t *testing.T) {
	old := PageTextFetcher
	PageTextFetcher = func(_ context.Context, u string, _ int) (string, error) {
		return strings.Repeat("字", 60), nil
	}
	defer func() { PageTextFetcher = old }()

	s := &stubSearcher{byQuery: map[string][]SearchResult{"q": {
		{Title: "1", URL: "https://e.example/1"},
		{Title: "2", URL: "https://e.example/2"},
		{Title: "3", URL: "https://e.example/3"},
	}}}
	b, err := DeepSearch(context.Background(), s, []string{"q"}, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Pages) != 2 {
		t.Fatalf("pages = %d, want 2", len(b.Pages))
	}
}

func TestDeepSearchUsesProviderRawContent(t *testing.T) {
	called := 0
	old := PageTextFetcher
	PageTextFetcher = func(context.Context, string, int) (string, error) {
		called++
		return "抓来的正文", nil
	}
	defer func() { PageTextFetcher = old }()

	s := &stubSearcher{byQuery: map[string][]SearchResult{"q": {
		{Title: "1", URL: "https://e.example/1", RawContent: "服务端抓好的全文"},
		{Title: "2", URL: "https://e.example/2"},
	}}}
	b, err := DeepSearch(context.Background(), s, []string{"q"}, 4)
	if err != nil {
		t.Fatal(err)
	}
	if got := b.Pages["https://e.example/1"]; got != "服务端抓好的全文" {
		t.Fatalf("provider raw_content not used: %q", got)
	}
	if got := b.Pages["https://e.example/2"]; got != "抓来的正文" {
		t.Fatalf("fallback fetch missing: %q", got)
	}
	if called != 1 {
		t.Fatalf("fetcher called %d times, want 1 (raw content must skip fetching)", called)
	}
}
