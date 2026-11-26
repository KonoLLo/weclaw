package agent

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	xhtml "golang.org/x/net/html"
)

const (
	DefaultDDGEndpoint = "https://html.duckduckgo.com/html/"
	ddgUserAgent       = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"
)

// DuckDuckGo scrapes the HTML endpoint. GET from datacenter IPs gets an
// "anomaly" challenge; the form POST used by the browser fallback works.
type DuckDuckGo struct {
	endpoint   string
	maxResults int
	client     *http.Client
}

func NewDuckDuckGo(endpoint string, maxResults int) *DuckDuckGo {
	if endpoint == "" {
		endpoint = DefaultDDGEndpoint
	}
	if maxResults <= 0 || maxResults > 10 {
		maxResults = DefaultSearchMaxResult
	}
	return &DuckDuckGo{
		endpoint:   endpoint,
		maxResults: maxResults,
		client:     &http.Client{Timeout: DefaultSearchTimeout},
	}
}

func (d *DuckDuckGo) Name() string { return "DuckDuckGo" }

func (d *DuckDuckGo) Search(ctx context.Context, query string) (string, []SearchResult, error) {
	q := strings.TrimSpace(query)
	if q == "" {
		return "", nil, fmt.Errorf("查询词是空的")
	}
	form := url.Values{}
	form.Set("q", q)
	form.Set("b", "")
	form.Set("kl", "wt-wt")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", ddgUserAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.5")
	resp, err := d.client.Do(req)
	if err != nil {
		return "", nil, fmt.Errorf("请求没发出去：%w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if resp.StatusCode == http.StatusAccepted || resp.StatusCode == http.StatusForbidden {
		return "", nil, fmt.Errorf("DuckDuckGo 拦了这次请求（HTTP %d），稍后再试", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return "", nil, fmt.Errorf("HTTP %d：%s", resp.StatusCode, trimSnippet(string(raw)))
	}
	results, err := parseDuckDuckGo(bytes.NewReader(raw), d.maxResults)
	if err != nil {
		return "", nil, err
	}
	if len(results) == 0 && bytes.Contains(bytes.ToLower(raw), []byte("anomaly")) {
		return "", nil, fmt.Errorf("DuckDuckGo 返回了反爬页面，稍后再试")
	}
	return "", results, nil
}

func parseDuckDuckGo(r io.Reader, max int) ([]SearchResult, error) {
	doc, err := xhtml.Parse(r)
	if err != nil {
		return nil, err
	}
	var out []SearchResult
	var walk func(*xhtml.Node)
	walk = func(n *xhtml.Node) {
		if len(out) >= max {
			return
		}
		if n.Type == xhtml.ElementNode && n.Data == "div" && hasClass(n, "result") {
			if res, ok := ddgResult(n); ok {
				out = append(out, res)
			}
			return
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	return out, nil
}

func ddgResult(n *xhtml.Node) (SearchResult, bool) {
	var res SearchResult
	var find func(*xhtml.Node)
	find = func(x *xhtml.Node) {
		if x.Type == xhtml.ElementNode {
			if x.Data == "a" && res.Title == "" && hasClass(x, "result__a") {
				res.Title = nodeText(x)
				res.URL = nodeAttr(x, "href")
			}
			if res.Content == "" && hasClass(x, "result__snippet") {
				res.Content = nodeText(x)
			}
		}
		for c := x.FirstChild; c != nil; c = c.NextSibling {
			find(c)
		}
	}
	find(n)
	res.Title = squashSpace(res.Title)
	res.Content = squashSpace(res.Content)
	return res, res.Title != ""
}

func hasClass(n *xhtml.Node, class string) bool {
	for _, f := range strings.Fields(nodeAttr(n, "class")) {
		if f == class {
			return true
		}
	}
	return false
}

func nodeAttr(n *xhtml.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

func nodeText(n *xhtml.Node) string {
	if n.Type == xhtml.TextNode {
		return n.Data
	}
	var b strings.Builder
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		b.WriteString(nodeText(c))
	}
	return b.String()
}

func squashSpace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
