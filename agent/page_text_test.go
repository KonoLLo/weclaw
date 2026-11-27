package agent

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	xhtml "golang.org/x/net/html"
	"golang.org/x/text/encoding/simplifiedchinese"
)

func TestNormalizePageText(t *testing.T) {
	in := "  第一段\t内容  \n\n\n\n第二段\u00a0内容  "
	want := "第一段 内容\n\n第二段 内容"
	if got := normalizePageText(in); got != want {
		t.Fatalf("normalizePageText() = %q, want %q", got, want)
	}
}

func TestHTMLToTextSkipsChromeAndScripts(t *testing.T) {
	src := `<html><head><title>页面标题</title><style>.a{}</style></head><body>` +
		`<nav>导航栏</nav><article><h1>大标题</h1><p>正文一</p><script>alert(1)</script>` +
		`<p>正文二&nbsp;结束</p></article><footer>页脚</footer></body></html>`
	doc, err := xhtml.Parse(strings.NewReader(src))
	if err != nil {
		t.Fatal(err)
	}
	got := normalizePageText(htmlToText(doc))
	for _, want := range []string{"大标题", "正文一", "正文二 结束"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in %q", want, got)
		}
	}
	for _, bad := range []string{"导航栏", "页脚", "alert(1)", "页面标题", ".a{}"} {
		if strings.Contains(got, bad) {
			t.Fatalf("leaked %q in %q", bad, got)
		}
	}
}

func TestFetchPageTextUTF8(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, `<html><body><article><p>这是`+strings.Repeat("正文", 40)+`</p></article></body></html>`)
	}))
	defer srv.Close()
	got, err := fetchPageText(context.Background(), srv.URL, 100, true)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if !strings.Contains(got, "正文正文") {
		t.Fatalf("unexpected text: %q", got)
	}
	if n := len([]rune(got)); n > 101 {
		t.Fatalf("not truncated: %d runes", n)
	}
}

func TestFetchPageTextGBK(t *testing.T) {
	body := `<html><body><p>` + strings.Repeat("中文正文", 20) + `</p></body></html>`
	gbk, err := simplifiedchinese.GBK.NewEncoder().Bytes([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=gbk")
		_, _ = w.Write(gbk)
	}))
	defer srv.Close()
	got, err := fetchPageText(context.Background(), srv.URL, 200, true)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if !strings.Contains(got, "中文正文") {
		t.Fatalf("gbk page not decoded: %q", got)
	}
}

func TestFetchPageTextRejectsNonHTML(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		_, _ = io.WriteString(w, "%PDF-1.4")
	}))
	defer srv.Close()
	if _, err := fetchPageText(context.Background(), srv.URL, 100, true); err == nil {
		t.Fatal("pdf should be rejected")
	}
}

func TestFetchPageTextRejectsPrivateHosts(t *testing.T) {
	for _, u := range []string{
		"http://127.0.0.1:18011/health",
		"http://localhost/",
		"http://192.168.1.1/",
		"file:///etc/passwd",
	} {
		if _, err := FetchPageText(context.Background(), u, 100); err == nil {
			t.Fatalf("%s should be rejected", u)
		}
	}
}
