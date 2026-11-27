package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

const (
	DefaultTavilyEndpoint  = "https://api.tavily.com/search"
	DefaultSearchTimeout   = 20 * time.Second
	DefaultSearchMaxResult = 5
)

type SearchResult struct {
	Title      string `json:"title"`
	URL        string `json:"url"`
	Content    string `json:"content"`
	RawContent string `json:"raw_content,omitempty"`
}

var searchTagPattern = regexp.MustCompile(`<[^>]+>`)

func searchPlainText(s string) string {
	return strings.TrimSpace(html.UnescapeString(searchTagPattern.ReplaceAllString(s, "")))
}

// Searcher is the common surface for /s providers.
type Searcher interface {
	Name() string
	Search(ctx context.Context, query string) (string, []SearchResult, error)
}

// WebSearch is a thin Tavily client: one query in, an optional short answer
// plus ranked results out.
type WebSearch struct {
	endpoint   string
	apiKey     string
	maxResults int
	client     *http.Client
}

func NewWebSearch(apiKey, endpoint string, maxResults int) *WebSearch {
	if endpoint == "" {
		endpoint = DefaultTavilyEndpoint
	}
	if maxResults <= 0 || maxResults > 10 {
		maxResults = DefaultSearchMaxResult
	}
	return &WebSearch{
		endpoint:   endpoint,
		apiKey:     apiKey,
		maxResults: maxResults,
		client:     &http.Client{Timeout: DefaultSearchTimeout},
	}
}

func (w *WebSearch) Name() string { return "Tavily" }

func (w *WebSearch) Search(ctx context.Context, query string) (string, []SearchResult, error) {
	q := strings.TrimSpace(query)
	if q == "" {
		return "", nil, fmt.Errorf("查询词是空的")
	}
	body, _ := json.Marshal(map[string]any{
		"api_key":             w.apiKey,
		"query":               q,
		"search_depth":        "basic",
		"include_answer":      true,
		"include_raw_content": true,
		"max_results":         w.maxResults,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.endpoint, bytes.NewReader(body))
	if err != nil {
		return "", nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := w.client.Do(req)
	if err != nil {
		return "", nil, fmt.Errorf("请求没发出去：%w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	var out struct {
		Answer  string          `json:"answer"`
		Results []SearchResult  `json:"results"`
		Detail  json.RawMessage `json:"detail"`
		Error   string          `json:"error"`
	}
	_ = json.Unmarshal(raw, &out)
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return "", nil, fmt.Errorf("Tavily 不认这个 key（HTTP %d）", resp.StatusCode)
	}
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == 432 {
		return "", nil, fmt.Errorf("Tavily 额度用完了或太频繁（HTTP %d）", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return "", nil, fmt.Errorf("HTTP %d：%s", resp.StatusCode, trimSnippet(string(raw)))
	}
	if out.Error != "" {
		return "", nil, fmt.Errorf("Tavily 说：%s", trimSnippet(out.Error))
	}
	return strings.TrimSpace(out.Answer), out.Results, nil
}
