package messaging

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/net/html"
	"golang.org/x/net/html/charset"

	"github.com/fastclaw-ai/weclaw/agent"
	"github.com/fastclaw-ai/weclaw/ilink"
)

const (
	webFetchTimeout   = 20 * time.Second
	webSummaryTimeout = 90 * time.Second
	webPageMaxBytes   = 6 << 20
	webPageMaxChars   = 12000
	webPageMinChars   = 80
)

var (
	webURLPattern = regexp.MustCompile(`https?://[A-Za-z0-9\-._~:/?#\[\]@!$&()*+,;=%]+`)
	errWebBlocked = errors.New("web address is not allowed")

	blockedWebCIDRs = mustParseCIDRs(
		"0.0.0.0/8",
		"100.64.0.0/10",
		"192.0.0.0/24",
		"192.0.2.0/24",
		"198.18.0.0/15",
		"198.51.100.0/24",
		"203.0.113.0/24",
		"240.0.0.0/4",
	)
)

type webLinkRequest struct {
	URL      string
	Question string
}

type webPage struct {
	URL         string
	Title       string
	Description string
	Site        string
	Author      string
	Published   string
	Body        string
	Truncated   bool
}

func mustParseCIDRs(raw ...string) []*net.IPNet {
	out := make([]*net.IPNet, 0, len(raw))
	for _, s := range raw {
		_, n, err := net.ParseCIDR(s)
		if err != nil {
			panic(err)
		}
		out = append(out, n)
	}
	return out
}

// detectLinkRequest recognizes an explicit /read command, a bare URL, or a URL
// wrapped in wording that clearly asks for the page to be read.
func detectLinkRequest(text string) (webLinkRequest, bool) {
	t := strings.TrimSpace(text)
	if t == "" {
		return webLinkRequest{}, false
	}
	lower := strings.ToLower(t)
	for _, prefix := range []string{"/读", "/链接", "/网页", "/read", "/url"} {
		if lower == prefix {
			return webLinkRequest{}, true
		}
		if strings.HasPrefix(lower, prefix+" ") || strings.HasPrefix(lower, prefix+"：") || strings.HasPrefix(lower, prefix+":") {
			rest := strings.TrimSpace(t[len(prefix):])
			raw := trimURLPunctuation(webURLPattern.FindString(rest))
			if raw == "" {
				return webLinkRequest{}, true
			}
			return webLinkRequest{URL: raw, Question: cleanLinkQuestion(removeFirstURL(rest, raw))}, true
		}
	}

	match := webURLPattern.FindString(t)
	if match == "" {
		return webLinkRequest{}, false
	}
	raw := trimURLPunctuation(match)
	if strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://") {
		return webLinkRequest{URL: raw, Question: cleanLinkQuestion(strings.TrimSpace(t[len(match):]))}, true
	}
	if looksLikeLinkQuestion(t) {
		return webLinkRequest{URL: raw, Question: cleanLinkQuestion(removeFirstURL(t, match))}, true
	}
	return webLinkRequest{}, false
}

func trimURLPunctuation(raw string) string {
	return strings.TrimRightFunc(strings.TrimSpace(raw), func(r rune) bool {
		return strings.ContainsRune(".,;:!?)]}，。；：！？、）】》\"'", r)
	})
}

func removeFirstURL(text, raw string) string {
	if raw == "" {
		return text
	}
	out := strings.Replace(text, raw, " ", 1)
	return strings.TrimSpace(out)
}

func looksLikeLinkQuestion(text string) bool {
	if utf8.RuneCountInString(text) > 300 {
		return false
	}
	low := strings.ToLower(text)
	for _, hint := range []string{
		"看看", "看一下", "看下", "读一下", "读读", "读一读", "总结", "提炼",
		"讲了什么", "说什么", "说了什么", "要点", "这篇文章", "这个链接",
		"这个网页", "这篇", "链接", "文章", "网页", "内容", "展开",
		"summarize", "summary", "read this", "this link",
	} {
		if strings.Contains(low, hint) {
			return true
		}
	}
	return false
}

func cleanLinkQuestion(s string) string {
	s = strings.TrimSpace(s)
	s = trimURLPunctuation(s)
	for _, prefix := range []string{"帮我", "请", "麻烦", "帮忙", "看一下", "看看", "读一下", "读一读", "总结一下", "提炼一下"} {
		s = strings.TrimSpace(strings.TrimPrefix(s, prefix))
	}
	s = strings.Trim(s, " ，,。.：:；;！!？?")
	if utf8.RuneCountInString(s) > 200 {
		s = string([]rune(s)[:200])
	}
	return s
}

func (h *Handler) handleLinkText(ctx context.Context, client *ilink.Client, msg ilink.WeixinMessage, text, clientID string) bool {
	req, ok := detectLinkRequest(text)
	if !ok {
		return false
	}
	if req.URL == "" {
		_ = SendTextReply(ctx, client, msg.FromUserID,
			"发一个 http/https 链接给我，并说要不要总结；例如：/读 https://example.com",
			msg.ContextToken, clientID)
		return true
	}

	_ = SendTypingState(ctx, client, msg.FromUserID, msg.ContextToken)
	log.Printf("[web] fetching link for %s: %s", msg.FromUserID, req.URL)

	page, err := fetchWebPage(ctx, req.URL)
	if err != nil {
		log.Printf("[web] fetch failed for %s: %v", req.URL, err)
		_ = SendTextReply(ctx, client, msg.FromUserID, friendlyWebError(err), msg.ContextToken, clientID)
		return true
	}
	if utf8.RuneCountInString(strings.TrimSpace(page.Body)) < webPageMinChars {
		_ = SendTextReply(ctx, client, msg.FromUserID,
			"这个页面没抓到正文，可能要先登录，或者正文是脚本加载的。换一个公开链接试试。",
			msg.ContextToken, clientID)
		return true
	}

	summary := h.summarizeWebPage(ctx, page, req.Question)
	if summary == "" {
		excerpt := page.Body
		if utf8.RuneCountInString(excerpt) > 500 {
			excerpt = string([]rune(excerpt)[:500]) + "…"
		}
		summary = "摘要没生成出来，先给你正文开头：\n" + excerpt
	}
	reply := buildWebPageReply(page, summary)
	if err := SendTextReply(ctx, client, msg.FromUserID, reply, msg.ContextToken, clientID); err != nil {
		log.Printf("[web] failed to send reply to %s: %v", msg.FromUserID, err)
	}
	return true
}

func buildWebPageReply(page *webPage, summary string) string {
	lines := make([]string, 0, 8)
	if title := strings.TrimSpace(page.Title); title != "" {
		lines = append(lines, "《"+title+"》")
	}
	for _, line := range strings.Split(summary, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			lines = append(lines, line)
		}
	}
	if len(lines) == 0 {
		lines = append(lines, "读完了，但没生成出摘要。")
	}
	return modJoin(lines)
}

func (h *Handler) summarizeWebPage(ctx context.Context, page *webPage, question string) string {
	ag := h.getDefaultAgent()
	if ag == nil {
		return ""
	}

	var sb strings.Builder
	if page.Title != "" {
		fmt.Fprintf(&sb, "标题：%s\n", page.Title)
	}
	if page.Site != "" {
		fmt.Fprintf(&sb, "站点：%s\n", page.Site)
	}
	body := []rune(page.Body)
	if len(body) > webPageMaxChars {
		body = body[:webPageMaxChars]
	}
	fmt.Fprintf(&sb, "正文：\n%s", string(body))

	var prompt strings.Builder
	prompt.WriteString("下面是网页正文。请用中文提炼 3 到 6 条要点，每条单独一行并加 1. 2. 3. 编号；")
	prompt.WriteString("只根据正文写，不要编造，不要输出网址或 Markdown 符号，总字数控制在 500 字以内。")
	if strings.TrimSpace(question) != "" {
		fmt.Fprintf(&prompt, "\n用户还问：%s\n请先结合正文回答这个问题，正文没提到就直说“正文里没写”。", strings.TrimSpace(question))
	}
	prompt.WriteString("\n\n")
	prompt.WriteString(sb.String())

	sctx, cancel := context.WithTimeout(ctx, webSummaryTimeout)
	defer cancel()
	var out string
	var err error
	if cc, ok := ag.(agent.CleanChatter); ok {
		out, err = cc.ChatClean(sctx, prompt.String())
	} else {
		_, _ = ag.ResetSession(sctx, "web-summary")
		out, err = ag.Chat(sctx, "web-summary", prompt.String())
	}
	if err != nil {
		log.Printf("[web] summary failed: %v", err)
		return ""
	}
	out = strings.TrimSpace(MarkdownToPlainText(out))
	if utf8.RuneCountInString(out) > 800 {
		out = string([]rune(out)[:800]) + "…"
	}
	return out
}

func friendlyWebError(err error) string {
	if errors.Is(err, errWebBlocked) {
		return "这个地址不允许读取（只支持公开的 http/https 网页）。"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "页面加载超时了，稍后再试，或换一个链接。"
	}
	return "这个链接没读成，可能打不开、要登录，或者站点不让抓取。"
}

func validateWebURL(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > 2048 {
		return nil, errWebBlocked
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return nil, errWebBlocked
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, errWebBlocked
	}
	if u.User != nil {
		return nil, errWebBlocked
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if host == "" {
		return nil, errWebBlocked
	}
	if host == "localhost" || strings.HasSuffix(host, ".localhost") ||
		strings.HasSuffix(host, ".local") || strings.HasSuffix(host, ".internal") {
		return nil, errWebBlocked
	}
	if !strings.Contains(host, ".") {
		return nil, errWebBlocked
	}
	if ip := net.ParseIP(host); ip != nil && isBlockedWebIP(ip) {
		return nil, errWebBlocked
	}
	if port := u.Port(); port != "" {
		for _, r := range port {
			if r < '0' || r > '9' {
				return nil, errWebBlocked
			}
		}
	}
	return u, nil
}

func isBlockedWebIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast() {
		return true
	}
	for _, cidr := range blockedWebCIDRs {
		if cidr.Contains(ip) {
			return true
		}
	}
	return false
}

func newWebFetchClient(timeout time.Duration) *http.Client {
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	transport := &http.Transport{
		// No ProxyFromEnvironment: a proxy could connect to an internal address
		// without passing through the public-IP check below.
		Proxy:                 nil,
		DialContext:           safeWebDialContext(dialer),
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 15 * time.Second,
		IdleConnTimeout:       30 * time.Second,
		MaxIdleConns:          4,
		ForceAttemptHTTP2:     true,
	}
	return &http.Client{
		Transport: transport,
		Timeout:   timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return fmt.Errorf("重定向次数过多")
			}
			if _, err := validateWebURL(req.URL.String()); err != nil {
				return err
			}
			return nil
		},
	}
}

func safeWebDialContext(dialer *net.Dialer) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, errWebBlocked
		}
		if ip := net.ParseIP(host); ip != nil {
			if isBlockedWebIP(ip) {
				return nil, errWebBlocked
			}
			return dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
		}

		ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, err
		}
		var lastErr error
		blocked := false
		for _, candidate := range ips {
			if isBlockedWebIP(candidate.IP) {
				blocked = true
				continue
			}
			conn, dialErr := dialer.DialContext(ctx, network, net.JoinHostPort(candidate.IP.String(), port))
			if dialErr == nil {
				return conn, nil
			}
			lastErr = dialErr
		}
		if lastErr != nil {
			return nil, lastErr
		}
		if blocked {
			return nil, errWebBlocked
		}
		return nil, fmt.Errorf("no address for %s", host)
	}
}

func fetchWebPage(parent context.Context, rawURL string) (*webPage, error) {
	u, err := validateWebURL(rawURL)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(parent, webFetchTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, errWebBlocked
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,text/plain;q=0.9,*/*;q=0.5")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.7")

	client := newWebFetchClient(webFetchTimeout)
	resp, err := client.Do(req)
	if err != nil {
		if errors.Is(err, errWebBlocked) {
			return nil, errWebBlocked
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, fmt.Errorf("fetch page: %w", context.DeadlineExceeded)
		}
		return nil, fmt.Errorf("fetch page: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("page returned HTTP %d", resp.StatusCode)
	}
	contentType := strings.ToLower(resp.Header.Get("Content-Type"))
	if contentType != "" && !strings.Contains(contentType, "html") &&
		!strings.Contains(contentType, "xml") && !strings.HasPrefix(contentType, "text/") {
		return nil, fmt.Errorf("unsupported content type %q", contentType)
	}

	raw, err := io.ReadAll(io.LimitReader(resp.Body, webPageMaxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read page: %w", err)
	}
	truncated := false
	if len(raw) > webPageMaxBytes {
		raw = raw[:webPageMaxBytes]
		truncated = true
	}

	reader, err := charset.NewReader(bytes.NewReader(raw), contentType)
	if err != nil {
		reader = bytes.NewReader(raw)
	}
	doc, err := html.Parse(reader)
	if err != nil {
		return nil, fmt.Errorf("parse page: %w", err)
	}

	page := extractWebPage(doc, resp.Request.URL.String())
	page.Truncated = page.Truncated || truncated
	if utf8.RuneCountInString(page.Body) < webPageMinChars && !isWeChatURL(rawURL) {
		// JS-heavy pages often have no usable HTML; Jina Reader is an optional
		// fallback for public pages. It has its own timeout and may be rate-limited.
		if meta, jinaErr := FetchViaJina(ctx, rawURL); jinaErr == nil {
			body := normalizeWebText(meta.Body)
			if utf8.RuneCountInString(body) > utf8.RuneCountInString(page.Body) {
				page.Body = capWebBody(body, &page.Truncated)
				if page.Title == "" {
					page.Title = strings.TrimSpace(meta.Title)
				}
			}
		} else {
			log.Printf("[web] direct page had little text; Jina fallback failed: %v", jinaErr)
		}
	}
	return page, nil
}

func extractWebPage(doc *html.Node, pageURL string) *webPage {
	page := &webPage{URL: pageURL}
	var docTitle, firstH1 string

	var walkMeta func(*html.Node)
	walkMeta = func(n *html.Node) {
		if n.Type == html.ElementNode {
			switch n.Data {
			case "title":
				if docTitle == "" && n.FirstChild != nil {
					docTitle = strings.TrimSpace(n.FirstChild.Data)
				}
			case "h1":
				if firstH1 == "" {
					firstH1 = strings.TrimSpace(nodeText(n))
				}
			case "meta":
				property := strings.ToLower(strings.TrimSpace(attrValue(n, "property")))
				name := strings.ToLower(strings.TrimSpace(attrValue(n, "name")))
				content := strings.TrimSpace(attrValue(n, "content"))
				switch {
				case content == "":
				case (property == "og:title" || name == "og:title") && page.Title == "":
					page.Title = content
				case (property == "og:description" || name == "description") && page.Description == "":
					page.Description = content
				case (property == "og:site_name") && page.Site == "":
					page.Site = content
				case name == "author" && page.Author == "":
					page.Author = content
				case property == "article:published_time" && page.Published == "":
					page.Published = content
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walkMeta(c)
		}
	}
	walkMeta(doc)

	if page.Title == "" {
		page.Title = docTitle
	}
	if page.Title == "" {
		page.Title = firstH1
	}
	if page.Title == "" {
		if u, err := url.Parse(pageURL); err == nil {
			page.Title = u.Host
		}
	}
	if page.Site == "" {
		if u, err := url.Parse(pageURL); err == nil {
			page.Site = u.Host
		}
	}

	root := pickContentRoot(doc)
	body := normalizeWebText(readableNodeText(root))
	if utf8.RuneCountInString(body) < webPageMinChars && page.Description != "" {
		body = page.Description
	}
	page.Body = capWebBody(body, &page.Truncated)
	return page
}

func attrValue(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if strings.EqualFold(a.Key, key) {
			return a.Val
		}
	}
	return ""
}

func nodeText(n *html.Node) string {
	var sb strings.Builder
	walkNodeText(n, &sb)
	return sb.String()
}

func pickContentRoot(doc *html.Node) *html.Node {
	var body *html.Node
	best := (*html.Node)(nil)
	bestScore := 0

	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			if n.Data == "body" {
				body = n
			}
			if isContentRootCandidate(n) {
				score := utf8.RuneCountInString(normalizeWebText(readableNodeText(n)))
				if n.Data == "article" || strings.EqualFold(attrValue(n, "id"), "js_content") {
					score *= 2
				}
				if score > bestScore && score >= webPageMinChars {
					best = n
					bestScore = score
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	if best != nil {
		return best
	}
	if body != nil {
		return body
	}
	return doc
}

func isContentRootCandidate(n *html.Node) bool {
	if n.Type != html.ElementNode {
		return false
	}
	switch n.Data {
	case "article", "main":
		return true
	}
	id := strings.ToLower(strings.TrimSpace(attrValue(n, "id")))
	class := strings.ToLower(attrValue(n, "class"))
	for _, s := range []string{
		"js_content", "article", "artibody", "article-content", "article-body",
		"post-content", "rich_media_content", "entry-content", "content-body",
		"main-content",
	} {
		if id == s || strings.Contains(class, s) {
			return true
		}
	}
	return false
}

var webSkipTags = map[string]bool{
	"script": true, "style": true, "noscript": true, "iframe": true,
	"svg": true, "canvas": true, "template": true, "nav": true,
	"header": true, "footer": true, "aside": true, "form": true,
	"button": true, "select": true, "textarea": true, "input": true,
}

var webBlockTags = map[string]bool{
	"p": true, "div": true, "section": true, "article": true, "main": true,
	"blockquote": true, "pre": true, "ul": true, "ol": true, "li": true,
	"h1": true, "h2": true, "h3": true, "h4": true, "h5": true, "h6": true,
	"tr": true, "table": true, "figure": true, "figcaption": true,
}

func readableNodeText(n *html.Node) string {
	var sb strings.Builder
	walkNodeText(n, &sb)
	return sb.String()
}

func walkNodeText(n *html.Node, sb *strings.Builder) {
	if n.Type == html.TextNode {
		sb.WriteString(n.Data)
		return
	}
	if n.Type != html.ElementNode {
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walkNodeText(c, sb)
		}
		return
	}

	tag := strings.ToLower(n.Data)
	if webSkipTags[tag] || isHiddenNode(n) {
		return
	}
	if tag == "br" {
		sb.WriteByte('\n')
		return
	}
	if tag == "li" {
		sb.WriteString("- ")
	}
	if tag == "td" || tag == "th" {
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walkNodeText(c, sb)
		}
		sb.WriteByte('\t')
		return
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		walkNodeText(c, sb)
	}
	if webBlockTags[tag] {
		sb.WriteByte('\n')
	}
}

func isHiddenNode(n *html.Node) bool {
	for _, a := range n.Attr {
		key := strings.ToLower(a.Key)
		val := strings.ToLower(strings.TrimSpace(a.Val))
		if key == "hidden" {
			return true
		}
		if key == "aria-hidden" && val == "true" {
			return true
		}
		if key == "style" && (strings.Contains(val, "display:none") ||
			strings.Contains(val, "display: none")) {
			return true
		}
	}
	return false
}

func normalizeWebText(raw string) string {
	raw = strings.ReplaceAll(raw, "\r\n", "\n")
	raw = strings.ReplaceAll(raw, "\r", "\n")
	raw = strings.ReplaceAll(raw, "\u00a0", " ")
	raw = strings.ReplaceAll(raw, "\u200b", "")

	lines := strings.Split(raw, "\n")
	out := make([]string, 0, len(lines))
	blank := false
	for _, line := range lines {
		line = strings.Join(strings.Fields(line), " ")
		if line == "" {
			if len(out) > 0 {
				blank = true
			}
			continue
		}
		if blank {
			out = append(out, "")
			blank = false
		}
		out = append(out, line)
	}
	for len(out) > 0 && out[len(out)-1] == "" {
		out = out[:len(out)-1]
	}
	return strings.Join(out, "\n")
}

func capWebBody(body string, truncated *bool) string {
	body = strings.TrimSpace(body)
	runes := []rune(body)
	if len(runes) <= webPageMaxChars {
		return body
	}
	*truncated = true
	return strings.TrimSpace(string(runes[:webPageMaxChars])) + "…"
}
