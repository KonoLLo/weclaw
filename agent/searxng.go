package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
)

// DefaultSearchTimeRange is empty so the 合并 source really queries both
// backends. A non-empty range makes SearXNG drop engines that cannot filter
// dates, so /se time is opt-in. SearXNG only offers day/week/month/year;
// there is no three-day window.
const DefaultSearchTimeRange = ""

// NormalizeSearchTimeRange maps user input to SearXNG's time_range values.
// Empty, off and 不限 all disable the filter.
func NormalizeSearchTimeRange(v string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "off", "none", "关", "关闭", "不限", "全部":
		return "", nil
	case "day", "d", "1d", "24h", "天", "今天", "一天":
		return "day", nil
	case "week", "w", "7d", "周", "一周", "7天":
		return "week", nil
	case "month", "m", "30d", "月", "一个月", "30天":
		return "month", nil
	case "year", "y", "1y", "年", "一年":
		return "year", nil
	}
	return "", fmt.Errorf("时间范围只能是 day、week、month、year 或 off")
}

// SearXNG talks to a self-hosted instance over its JSON endpoint. The instance
// must list json in search.formats; otherwise SearXNG answers with HTML.
// time_range is mutable at runtime, so it is guarded by mu.
type SearXNG struct {
	baseURL    string
	auth       string
	maxResults int
	client     *http.Client

	mu        sync.RWMutex
	timeRange string
}

func NewSearXNG(baseURL, auth string, maxResults int) *SearXNG {
	if maxResults <= 0 || maxResults > 20 {
		maxResults = DefaultSearchMaxResult
	}
	return &SearXNG{
		baseURL:    strings.TrimSpace(baseURL),
		auth:       strings.TrimSpace(auth),
		maxResults: maxResults,
		client:     &http.Client{Timeout: DefaultSearchTimeout},
		timeRange:  DefaultSearchTimeRange,
	}
}

func (s *SearXNG) SetTimeRange(v string) error {
	tr, err := NormalizeSearchTimeRange(v)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.timeRange = tr
	s.mu.Unlock()
	return nil
}

func (s *SearXNG) TimeRange() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.timeRange
}

func (s *SearXNG) Name() string { return "SearXNG" }

func (s *SearXNG) Search(ctx context.Context, query string) (string, []SearchResult, error) {
	q := strings.TrimSpace(query)
	if q == "" {
		return "", nil, fmt.Errorf("查询词是空的")
	}
	if s.baseURL == "" {
		return "", nil, fmt.Errorf("没配 searxng_url")
	}
	u, err := url.Parse(s.baseURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", nil, fmt.Errorf("searxng_url 不是完整网址")
	}
	if !strings.HasSuffix(strings.TrimRight(u.Path, "/"), "/search") {
		u.Path = strings.TrimRight(u.Path, "/") + "/search"
	}
	values := u.Query()
	values.Set("q", q)
	values.Set("format", "json")
	values.Set("safesearch", "0")
	if tr := s.TimeRange(); tr != "" {
		values.Set("time_range", tr)
	}
	u.RawQuery = values.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", nil, err
	}
	req.Header.Set("Accept", "application/json")
	if auth := searxngAuthHeader(s.auth); auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return "", nil, fmt.Errorf("请求没发出去：%w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))

	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return "", nil, fmt.Errorf("SearXNG 不认这个访问口令（HTTP %d）", resp.StatusCode)
	case resp.StatusCode == http.StatusForbidden:
		if looksLikeHTML(resp.Header.Get("Content-Type"), raw) {
			return "", nil, fmt.Errorf("SearXNG 拒绝访问（HTTP 403）：口令没配错的话，请确认 settings.yml 的 search.formats 包含 json")
		}
		return "", nil, fmt.Errorf("SearXNG 不认这个访问口令（HTTP %d）", resp.StatusCode)
	case resp.StatusCode == http.StatusTooManyRequests:
		return "", nil, fmt.Errorf("SearXNG 限流了（HTTP %d）", resp.StatusCode)
	case resp.StatusCode != http.StatusOK:
		return "", nil, fmt.Errorf("HTTP %d：%s", resp.StatusCode, trimSnippet(string(raw)))
	}

	var out struct {
		Answers []string `json:"answers"`
		Results []struct {
			Title   string `json:"title"`
			URL     string `json:"url"`
			Content string `json:"content"`
		} `json:"results"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		if looksLikeHTML(resp.Header.Get("Content-Type"), raw) {
			return "", nil, fmt.Errorf("SearXNG 没开 JSON 输出：在 settings.yml 的 search.formats 里加上 json")
		}
		return "", nil, fmt.Errorf("SearXNG 返回了看不懂的内容：%s", trimSnippet(string(raw)))
	}

	results := make([]SearchResult, 0, len(out.Results))
	for _, r := range out.Results {
		if len(results) >= s.maxResults {
			break
		}
		if r.URL == "" || (r.Title == "" && r.Content == "") {
			continue
		}
		results = append(results, SearchResult{
			Title:   searchPlainText(r.Title),
			URL:     r.URL,
			Content: searchPlainText(r.Content),
		})
	}
	answer := strings.TrimSpace(strings.Join(out.Answers, "\n"))
	return searchPlainText(answer), results, nil
}

func searxngAuthHeader(v string) string {
	if v == "" {
		return ""
	}
	low := strings.ToLower(v)
	if strings.HasPrefix(low, "bearer ") || strings.HasPrefix(low, "basic ") {
		return v
	}
	return "Bearer " + v
}

func looksLikeHTML(contentType string, body []byte) bool {
	if strings.Contains(strings.ToLower(contentType), "text/html") {
		return true
	}
	return strings.HasPrefix(strings.TrimSpace(string(body)), "<")
}
