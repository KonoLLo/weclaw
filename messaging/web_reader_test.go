package messaging

import (
	"context"
	"errors"
	"strings"
	"testing"

	"golang.org/x/net/html"
)

func TestDetectLinkRequest(t *testing.T) {
	cases := []struct {
		text   string
		url    string
		quest  string
		wantOK bool
	}{
		{"https://example.com/a", "https://example.com/a", "", true},
		{"https://example.com/a 帮我总结一下", "https://example.com/a", "", true},
		{"/读 https://example.com/a", "https://example.com/a", "", true},
		{"/读：https://example.com/a 主要讲了什么", "https://example.com/a", "主要讲了什么", true},
		{"看看这个链接 https://example.com/a，讲什么", "https://example.com/a", "讲什么", true},
		{"帮我写个爬虫 https://example.com/a", "", "", false},
		{"今天天气不错", "", "", false},
		{"http://127.0.0.1:8080/admin", "http://127.0.0.1:8080/admin", "", true},
		{"/读", "", "", true},
	}
	for _, c := range cases {
		got, ok := detectLinkRequest(c.text)
		if ok != c.wantOK || got.URL != c.url {
			t.Errorf("detectLinkRequest(%q) = (%+v, %v), want url=%q ok=%v", c.text, got, ok, c.url, c.wantOK)
		}
		if ok && c.quest != "" && !strings.Contains(got.Question, c.quest) {
			t.Errorf("detectLinkRequest(%q) question = %q, want contains %q", c.text, got.Question, c.quest)
		}
	}
}

func TestValidateWebURLBlocksLocal(t *testing.T) {
	for _, raw := range []string{
		"http://127.0.0.1/",
		"http://localhost:8080/",
		"http://10.0.0.1/",
		"http://192.168.1.1/",
		"http://169.254.169.254/latest/meta-data",
		"http://[::1]/",
		"file:///etc/passwd",
		"ftp://example.com/file",
		"http://user:pass@example.com/",
		"http://intranet/",
	} {
		if _, err := validateWebURL(raw); !errors.Is(err, errWebBlocked) {
			t.Errorf("validateWebURL(%q) err = %v, want errWebBlocked", raw, err)
		}
	}
	if _, err := validateWebURL("https://example.com/a?b=1"); err != nil {
		t.Errorf("public URL rejected: %v", err)
	}
}

func TestFetchWebPageRejectsPrivateBeforeDial(t *testing.T) {
	if _, err := fetchWebPage(context.Background(), "http://127.0.0.1:9/"); !errors.Is(err, errWebBlocked) {
		t.Fatalf("fetchWebPage private err = %v, want errWebBlocked", err)
	}
}

func TestExtractWebPageArticle(t *testing.T) {
	raw := `<!doctype html>
<html><head>
<title>普通标题</title>
<meta property="og:title" content="正式标题">
<meta property="og:site_name" content="测试站点">
<meta name="description" content="这是页面描述，用来兜底。">
</head><body>
<nav>导航栏不该出现</nav>
<article>
<h1>文章大标题</h1>
<p>第一段正文，介绍这篇文章讲的是什么。` + strings.Repeat("这一段用来补足正文长度，方便内容区识别。", 4) + `</p>
<script>alert("脚本不该出现")</script>
<div id="js_content"><p>第二段正文，包含更多细节和结论。` + strings.Repeat("这里继续补充一些正文内容。", 4) + `</p><p style="display:none">隐藏内容不该出现</p></div>
</article>
<footer>页脚不该出现</footer>
</body></html>`
	doc, err := html.Parse(strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	page := extractWebPage(doc, "https://example.com/post")
	if page.Title != "正式标题" {
		t.Errorf("title = %q", page.Title)
	}
	if page.Site != "测试站点" {
		t.Errorf("site = %q", page.Site)
	}
	for _, want := range []string{"第一段正文", "第二段正文"} {
		if !strings.Contains(page.Body, want) {
			t.Errorf("body missing %q: %q", want, page.Body)
		}
	}
	for _, bad := range []string{"导航栏不该出现", "脚本不该出现", "隐藏内容不该出现", "页脚不该出现"} {
		if strings.Contains(page.Body, bad) {
			t.Errorf("body contains %q: %q", bad, page.Body)
		}
	}
}

func TestExtractWebPageWeChatContent(t *testing.T) {
	raw := `<!doctype html><html><head><title>公众号文章</title></head><body>
<div class="rich_media_area_primary">
  <h1>标题</h1>
  <div id="js_content">
    <p>` + strings.Repeat("这是公众号正文内容。", 10) + `</p>
    <p>结尾段落。</p>
  </div>
</div>
<div class="qr_code_pc">扫码关注不该出现</div>
</body></html>`
	doc, err := html.Parse(strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	page := extractWebPage(doc, "https://mp.weixin.qq.com/s/abc")
	if !strings.Contains(page.Body, "这是公众号正文内容") || !strings.Contains(page.Body, "结尾段落") {
		t.Fatalf("wechat body = %q", page.Body)
	}
	if strings.Contains(page.Body, "扫码关注") {
		t.Fatalf("wechat body includes outside content: %q", page.Body)
	}
}

func TestNormalizeWebTextAndCap(t *testing.T) {
	got := normalizeWebText("  第一行  \r\n\r\n\r\n 第二行\t内容  \n\n")
	if got != "第一行\n\n第二行 内容" {
		t.Fatalf("normalizeWebText = %q", got)
	}
	truncated := false
	body := capWebBody(strings.Repeat("字", webPageMaxChars+10), &truncated)
	if !truncated || !strings.HasSuffix(body, "…") {
		t.Fatalf("capWebBody did not truncate")
	}
}
