package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

const (
	DefaultBraveEndpoint = "https://api.search.brave.com/res/v1/web/search"
	braveUserAgent       = "weclaw/2 (+https://github.com/fastclaw-ai/weclaw)"
)

var braveTagPattern = regexp.MustCompile(`<[^>]+>`)

func bravePlainText(s string) string {
	return strings.TrimSpace(html.UnescapeString(braveTagPattern.ReplaceAllString(s, "")))
}

// BraveSearch is a thin client for the Brave Web Search API (free plan:
// 2000 queries/month, key required).
type BraveSearch struct {
	endpoint   string
	apiKey     string
	maxResults int
	client     *http.Client
}

func NewBraveSearch(apiKey, endpoint string, maxResults int) *BraveSearch {
	if endpoint == "" {
		endpoint = DefaultBraveEndpoint
	}
	if maxResults <= 0 || maxResults > 10 {
		maxResults = DefaultSearchMaxResult
	}
	return &BraveSearch{
		endpoint:   endpoint,
		apiKey:     apiKey,
		maxResults: maxResults,
		client:     &http.Client{Timeout: DefaultSearchTimeout},
	}
}

func (b *BraveSearch) Name() string { return "Brave" }

func (b *BraveSearch) Search(ctx context.Context, query string) (string, []SearchResult, error) {
	q := strings.TrimSpace(query)
	if q == "" {
		return "", nil, fmt.Errorf("查询词是空的")
	}
	u, err := url.Parse(b.endpoint)
	if err != nil {
		return "", nil, err
	}
	qs := u.Query()
	qs.Set("q", q)
	qs.Set("count", strconv.Itoa(b.maxResults))
	qs.Set("safesearch", "moderate")
	u.RawQuery = qs.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Subscription-Token", b.apiKey)
	req.Header.Set("User-Agent", braveUserAgent)

	resp, err := b.client.Do(req)
	if err != nil {
		return "", nil, fmt.Errorf("请求没发出去：%w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return "", nil, fmt.Errorf("Brave 不认这个 key（HTTP %d）", resp.StatusCode)
	case resp.StatusCode == http.StatusTooManyRequests:
		return "", nil, fmt.Errorf("Brave 额度用完了或太频繁（HTTP %d）", resp.StatusCode)
	case resp.StatusCode != http.StatusOK:
		return "", nil, fmt.Errorf("HTTP %d：%s", resp.StatusCode, trimSnippet(string(raw)))
	}

	var out struct {
		Web struct {
			Results []struct {
				Title       string `json:"title"`
				URL         string `json:"url"`
				Description string `json:"description"`
			} `json:"results"`
		} `json:"web"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", nil, fmt.Errorf("Brave 返回看不懂：%w", err)
	}
	results := make([]SearchResult, 0, len(out.Web.Results))
	for _, r := range out.Web.Results {
		title := strings.TrimSpace(r.Title)
		if title == "" {
			continue
		}
		results = append(results, SearchResult{
			Title:   bravePlainText(r.Title),
			URL:     r.URL,
			Content: bravePlainText(r.Description),
		})
	}
	return "", results, nil
}
