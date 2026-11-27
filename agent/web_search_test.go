package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestWebSearchParse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"answer":"答案在这","results":[{"title":"T1","url":"https://a.example/1","content":"c1","raw_content":"全文一"},{"title":"T2","url":"https://a.example/2","content":"c2"}]}`))
	}))
	defer srv.Close()
	w := NewWebSearch("k", srv.URL, 3)
	ans, res, err := w.Search(context.Background(), "今天天气")
	if err != nil || ans != "答案在这" || len(res) != 2 || res[0].URL != "https://a.example/1" {
		t.Fatalf("解析不对: ans=%q res=%+v err=%v", ans, res, err)
	}
	if res[0].RawContent != "全文一" {
		t.Fatalf("raw_content 没解析出来: %+v", res[0])
	}
	t.Logf("ok: answer=%q results=%d first=%s", ans, len(res), res[0].Title)
}

func TestWebSearchErrors(t *testing.T) {
	for code, want := range map[int]string{401: "不认这个 key", 432: "额度", 500: "HTTP 500"} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(code)
			w.Write([]byte(`{"detail":"x"}`))
		}))
		_, _, err := NewWebSearch("k", srv.URL, 3).Search(context.Background(), "q")
		srv.Close()
		if err == nil || !contains(err.Error(), want) {
			t.Errorf("HTTP %d: 期望含 %q，实际 %v", code, want, err)
		} else {
			t.Logf("HTTP %d -> %v", code, err)
		}
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
