package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBraveSearchParsesAndStripsTags(t *testing.T) {
	var gotQuery, gotToken string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query().Get("q")
		gotToken = r.Header.Get("X-Subscription-Token")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"web":{"results":[
			{"title":"上海<strong>天气</strong>","url":"https://example.com/a","description":"明天<strong>有雨</strong>，气温 18&deg;C"},
			{"title":"","url":"https://example.com/b","description":"no title"}
		]}}`))
	}))
	defer server.Close()

	b := NewBraveSearch("test-key", server.URL, 5)
	answer, results, err := b.Search(context.Background(), "上海 天气")
	if err != nil {
		t.Fatal(err)
	}
	if answer != "" {
		t.Fatalf("brave has no answer field, got %q", answer)
	}
	if gotQuery != "上海 天气" || gotToken != "test-key" {
		t.Fatalf("unexpected request: q=%q token=%q", gotQuery, gotToken)
	}
	if len(results) != 1 {
		t.Fatalf("empty titles should be dropped, got %#v", results)
	}
	if results[0].Title != "上海天气" {
		t.Fatalf("title not stripped: %q", results[0].Title)
	}
	if !strings.Contains(results[0].Content, "明天有雨") || strings.Contains(results[0].Content, "<strong>") {
		t.Fatalf("description not stripped: %q", results[0].Content)
	}
}

func TestBraveSearchErrors(t *testing.T) {
	cases := []struct {
		status int
		want   string
	}{
		{http.StatusUnauthorized, "不认这个 key"},
		{http.StatusTooManyRequests, "额度"},
		{http.StatusInternalServerError, "HTTP 500"},
	}
	for _, tc := range cases {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.status)
			_, _ = w.Write([]byte(`{"error":"nope"}`))
		}))
		_, _, err := NewBraveSearch("k", server.URL, 5).Search(context.Background(), "q")
		server.Close()
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("status %d: unexpected error %v", tc.status, err)
		}
	}
}
