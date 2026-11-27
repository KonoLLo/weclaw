package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	xhtml "golang.org/x/net/html"
	"golang.org/x/net/html/charset"
)

const (
	pageFetchTimeout  = 7 * time.Second
	pageFetchMaxBytes = 3 << 20
	pageTextMaxRunes  = 2400
)

var (
	errUnsafeURL = errors.New("不是公网 http(s) 链接，跳过")
	pageSpaceRE  = regexp.MustCompile(`[\t\r\f\v ]+`)
	pageBlankRE  = regexp.MustCompile(`\n{3,}`)
)

var pageSkipTags = map[string]bool{
	"script": true, "style": true, "noscript": true, "iframe": true,
	"svg": true, "canvas": true, "head": true, "nav": true, "footer": true,
	"header": true, "aside": true, "form": true, "button": true,
	"select": true, "option": true, "template": true,
}

var pageBlockTags = map[string]bool{
	"p": true, "div": true, "li": true, "ul": true, "ol": true, "br": true,
	"h1": true, "h2": true, "h3": true, "h4": true, "h5": true, "h6": true,
	"tr": true, "td": true, "th": true, "section": true, "article": true,
	"main": true, "blockquote": true, "pre": true, "table": true, "dd": true,
	"dt": true, "figcaption": true,
}

var pageUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"

// PageTextFetcher downloads readable text for a URL. It is a variable so
// callers and tests can substitute a stub.
var PageTextFetcher = FetchPageText

// FetchPageText downloads a search result page and returns its readable text.
// Non-HTML payloads, private hosts and empty pages are rejected so the caller
// can fall back to the search snippet.
func FetchPageText(ctx context.Context, rawURL string, maxRunes int) (string, error) {
	return fetchPageText(ctx, rawURL, maxRunes, false)
}

func fetchPageText(ctx context.Context, rawURL string, maxRunes int, allowPrivate bool) (string, error) {
	if maxRunes <= 0 {
		maxRunes = pageTextMaxRunes
	}
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return "", errUnsafeURL
	}
	if !allowPrivate {
		if err := checkPublicHost(ctx, u.Hostname()); err != nil {
			return "", err
		}
	}
	ctx, cancel := context.WithTimeout(ctx, pageFetchTimeout)
	defer cancel()

	client := &http.Client{
		Timeout: pageFetchTimeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("跳转次数太多")
			}
			if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
				return errUnsafeURL
			}
			return checkHostLiteral(req.URL.Hostname())
		},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", pageUserAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,text/plain;q=0.8")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.5")
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("打不开：%w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	ct := resp.Header.Get("Content-Type")
	lower := strings.ToLower(ct)
	isHTML := strings.Contains(lower, "html") || strings.Contains(lower, "xml") || lower == ""
	isPlain := strings.Contains(lower, "text/plain")
	if !isHTML && !isPlain {
		return "", fmt.Errorf("不是网页（%s）", ct)
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, pageFetchMaxBytes))
	if len(raw) == 0 {
		return "", errors.New("页面是空的")
	}
	decoded, err := charset.NewReader(bytes.NewReader(raw), ct)
	if err != nil {
		decoded = bytes.NewReader(raw)
	}
	var text string
	if isPlain {
		b, _ := io.ReadAll(io.LimitReader(decoded, pageFetchMaxBytes))
		text = normalizePageText(string(b))
	} else {
		doc, err := xhtml.Parse(decoded)
		if err != nil {
			return "", fmt.Errorf("网页解析失败：%w", err)
		}
		text = normalizePageText(htmlToText(doc))
	}
	if utf8.RuneCountInString(text) < 40 {
		return "", errors.New("正文太短")
	}
	return truncateText(text, maxRunes), nil
}

func htmlToText(doc *xhtml.Node) string {
	var b strings.Builder
	var walk func(*xhtml.Node)
	walk = func(n *xhtml.Node) {
		if n.Type == xhtml.ElementNode {
			tag := strings.ToLower(n.Data)
			if pageSkipTags[tag] {
				return
			}
			if tag == "br" {
				b.WriteString("\n")
			}
		}
		if n.Type == xhtml.TextNode {
			b.WriteString(n.Data)
			b.WriteString(" ")
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
		if n.Type == xhtml.ElementNode && pageBlockTags[strings.ToLower(n.Data)] {
			b.WriteString("\n")
		}
	}
	walk(doc)
	return b.String()
}

func normalizePageText(s string) string {
	s = strings.ReplaceAll(s, "\u00a0", " ")
	s = pageSpaceRE.ReplaceAllString(s, " ")
	lines := strings.Split(s, "\n")
	kept := make([]string, 0, len(lines))
	for _, ln := range lines {
		kept = append(kept, strings.TrimSpace(ln))
	}
	s = strings.Join(kept, "\n")
	s = pageBlankRE.ReplaceAllString(s, "\n\n")
	return strings.TrimSpace(s)
}

func truncateText(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}

// checkPublicHost rejects localhost and hosts that resolve to private ranges.
// DNS failures are left to the HTTP layer so the real error surfaces.
func checkPublicHost(ctx context.Context, host string) error {
	if host == "" {
		return errUnsafeURL
	}
	host = strings.Trim(host, "[]")
	if isLocalName(host) {
		return errUnsafeURL
	}
	if ip := net.ParseIP(host); ip != nil {
		if isPrivateIP(ip) {
			return errUnsafeURL
		}
		return nil
	}
	lctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	ips, err := net.DefaultResolver.LookupIPAddr(lctx, host)
	if err != nil {
		return nil
	}
	for _, a := range ips {
		if isPrivateIP(a.IP) {
			return errUnsafeURL
		}
	}
	return nil
}

func checkHostLiteral(host string) error {
	host = strings.Trim(host, "[]")
	if isLocalName(host) {
		return errUnsafeURL
	}
	if ip := net.ParseIP(host); ip != nil && isPrivateIP(ip) {
		return errUnsafeURL
	}
	return nil
}

func isLocalName(host string) bool {
	h := strings.ToLower(host)
	return h == "localhost" || strings.HasSuffix(h, ".localhost")
}

func isPrivateIP(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast()
}
