package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSearXNGSearchParsesResults(t *testing.T) {
	var gotPath, gotQuery, gotFormat, gotAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.Query().Get("q")
		gotFormat = r.URL.Query().Get("format")
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"answers":["明天<strong>有雨</strong>"],"results":[
			{"title":"上海<strong>天气</strong>","url":"https://example.com/a","content":"气温 18&deg;C"},
			{"title":"空链接","url":"","content":"drop me"},
			{"title":"第二页","url":"https://example.com/b","content":""}
		]}`))
	}))
	defer server.Close()

	s := NewSearXNG(server.URL+"/searx", "test-token", 5)
	answer, results, err := s.Search(context.Background(), "上海 天气")
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/searx/search" || gotQuery != "上海 天气" || gotFormat != "json" {
		t.Fatalf("unexpected request: path=%q q=%q format=%q", gotPath, gotQuery, gotFormat)
	}
	if gotAuth != "Bearer test-token" {
		t.Fatalf("auth = %q", gotAuth)
	}
	if answer != "明天有雨" {
		t.Fatalf("answer = %q", answer)
	}
	if len(results) != 2 {
		t.Fatalf("empty-url results should be dropped, got %#v", results)
	}
	if results[0].Title != "上海天气" || results[0].Content != "气温 18°C" {
		t.Fatalf("tags not stripped: %#v", results[0])
	}
}

func TestSearXNGKeepsBasicAuth(t *testing.T) {
	var gotAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[]}`))
	}))
	defer server.Close()

	if _, _, err := NewSearXNG(server.URL, "Basic dXNlcjpwYXNz", 5).Search(context.Background(), "q"); err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Basic dXNlcjpwYXNz" {
		t.Fatalf("auth = %q", gotAuth)
	}
}

func TestSearXNGJSONDisabled(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<html><body>JSON format is not enabled</body></html>`))
	}))
	defer server.Close()

	_, _, err := NewSearXNG(server.URL, "", 5).Search(context.Background(), "q")
	if err == nil || !strings.Contains(err.Error(), "没开 JSON") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestSearXNGForbiddenJSONHint(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`<html><body>JSON format is not enabled</body></html>`))
	}))
	defer server.Close()

	_, _, err := NewSearXNG(server.URL, "", 5).Search(context.Background(), "q")
	if err == nil || !strings.Contains(err.Error(), "search.formats") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestSearXNGTimeRange(t *testing.T) {
	var gotTimeRange string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotTimeRange = r.URL.Query().Get("time_range")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[]}`))
	}))
	defer server.Close()

	s := NewSearXNG(server.URL, "", 5)
	if s.TimeRange() != "" {
		t.Fatalf("default time range = %q, want empty", s.TimeRange())
	}
	if _, _, err := s.Search(context.Background(), "q"); err != nil {
		t.Fatal(err)
	}
	if gotTimeRange != "" {
		t.Fatalf("time_range sent = %q, want none by default", gotTimeRange)
	}

	if err := s.SetTimeRange("week"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Search(context.Background(), "q"); err != nil {
		t.Fatal(err)
	}
	if gotTimeRange != "week" {
		t.Fatalf("time_range sent = %q, want week", gotTimeRange)
	}

	if err := s.SetTimeRange("day"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Search(context.Background(), "q"); err != nil {
		t.Fatal(err)
	}
	if gotTimeRange != "day" {
		t.Fatalf("time_range sent = %q, want day", gotTimeRange)
	}

	if err := s.SetTimeRange("off"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Search(context.Background(), "q"); err != nil {
		t.Fatal(err)
	}
	if gotTimeRange != "" {
		t.Fatalf("time_range sent = %q, want empty after off", gotTimeRange)
	}

	if err := s.SetTimeRange("去年"); err == nil {
		t.Fatal("invalid range should fail")
	}
	if s.TimeRange() != "" {
		t.Fatalf("invalid input must not change state, got %q", s.TimeRange())
	}
}

func TestSearXNGErrors(t *testing.T) {
	cases := []struct {
		status   int
		contains string
	}{
		{http.StatusUnauthorized, "访问口令"},
		{http.StatusTooManyRequests, "限流"},
		{http.StatusBadGateway, "HTTP 502"},
	}
	for _, tc := range cases {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.status)
			_, _ = w.Write([]byte(`down`))
		}))
		_, _, err := NewSearXNG(server.URL, "", 5).Search(context.Background(), "q")
		server.Close()
		if err == nil || !strings.Contains(err.Error(), tc.contains) {
			t.Fatalf("status %d: unexpected error %v", tc.status, err)
		}
	}
}
