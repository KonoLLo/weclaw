package messaging

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/fastclaw-ai/weclaw/agent"
)

type fakeCleanAgent struct {
	fakeAgent
	replies []string
	prompts []string
}

func (f *fakeCleanAgent) ChatClean(_ context.Context, message string) (string, error) {
	f.prompts = append(f.prompts, message)
	if len(f.replies) == 0 {
		return "", errors.New("no scripted reply")
	}
	out := f.replies[0]
	f.replies = f.replies[1:]
	return out, nil
}

type fakeSearcher struct {
	results []agent.SearchResult
	answer  string
	err     error
	queries []string
}

func (f *fakeSearcher) Name() string { return "fake" }

func (f *fakeSearcher) Search(_ context.Context, query string) (string, []agent.SearchResult, error) {
	f.queries = append(f.queries, query)
	return f.answer, f.results, f.err
}

func TestParseAutoSearchDecision(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		queries []string
		need    bool
	}{
		{"plain json", `{"search":true,"query":"上海 天气"}`, []string{"上海 天气"}, true},
		{"code fence", "```json\n{\"search\":true,\"query\":\"GPT-5 发布\"}\n```", []string{"GPT-5 发布"}, true},
		{"multi queries", `{"search":true,"queries":["上海 天气","上海 降雨"]}`, []string{"上海 天气", "上海 降雨"}, true},
		{"query plus queries dedup", `{"search":true,"query":"A","queries":["A","B"]}`, []string{"A", "B"}, true},
		{"caps at three", `{"search":true,"queries":["1","2","3","4"]}`, []string{"1", "2", "3"}, true},
		{"no search", `{"search":false}`, nil, false},
		{"missing query", `{"search":true}`, nil, false},
		{"garbage", "我觉得不用搜", nil, false},
		{"empty", "", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			qs, need := parseAutoSearchDecision(tc.raw)
			if need != tc.need {
				t.Fatalf("need = %v, want %v", need, tc.need)
			}
			if len(qs) != len(tc.queries) {
				t.Fatalf("queries = %#v, want %#v", qs, tc.queries)
			}
			for i := range qs {
				if qs[i] != tc.queries[i] {
					t.Fatalf("queries = %#v, want %#v", qs, tc.queries)
				}
			}
		})
	}
}

func TestParseAutoSearchDecisionTruncatesLongQuery(t *testing.T) {
	long := strings.Repeat("词", 120)
	qs, need := parseAutoSearchDecision(`{"search":true,"queries":["` + long + `"]}`)
	if !need || len(qs) != 1 {
		t.Fatalf("unexpected decision: %#v %v", qs, need)
	}
	if n := len([]rune(qs[0])); n != autoSearchMaxQuery {
		t.Fatalf("truncated query runes = %d, want %d", n, autoSearchMaxQuery)
	}
}

func TestAutoSearchPromptInjectsDistilledContext(t *testing.T) {
	old := agent.PageTextFetcher
	agent.PageTextFetcher = func(_ context.Context, u string, _ int) (string, error) {
		return "网页正文：" + u + " " + strings.Repeat("内容", 40), nil
	}
	defer func() { agent.PageTextFetcher = old }()

	searcher := &fakeSearcher{
		results: []agent.SearchResult{{Title: "天气预报", URL: "https://example.com/weather", Content: "明天上海有雨"}},
		answer:  "上海明天有雨",
	}
	h := &Handler{webSearch: searcher, autoSearch: true}
	ag := &fakeCleanAgent{replies: []string{
		`{"search":true,"queries":["上海 明天 天气","上海 降雨"]}`,
		"明天上海有雨，记得带伞。",
	}}

	got := h.autoSearchPrompt(context.Background(), "u1", "明天去上海要带伞吗", ag)
	if !strings.Contains(got, "联网查到的资料") || !strings.Contains(got, "记得带伞") {
		t.Fatalf("unexpected context block: %q", got)
	}
	if len(searcher.queries) != 2 || searcher.queries[0] != "上海 明天 天气" || searcher.queries[1] != "上海 降雨" {
		t.Fatalf("unexpected search queries: %#v", searcher.queries)
	}
	if len(ag.prompts) != 2 {
		t.Fatalf("expected router + digest calls, got %d", len(ag.prompts))
	}
	if !strings.Contains(ag.prompts[1], "网页正文") {
		t.Fatalf("digest prompt should carry fetched page text: %q", ag.prompts[1])
	}
}

func TestAutoSearchPromptSkipsChatAndDisabled(t *testing.T) {
	searcher := &fakeSearcher{}
	h := &Handler{webSearch: searcher, autoSearch: true}
	ag := &fakeCleanAgent{replies: []string{`{"search":false}`}}
	if got := h.autoSearchPrompt(context.Background(), "u1", "哈哈哈哈今天好开心", ag); got != "" {
		t.Fatalf("chat turn should not inject: %q", got)
	}
	if len(searcher.queries) != 0 {
		t.Fatalf("chat turn should not search: %#v", searcher.queries)
	}

	off := &Handler{webSearch: searcher, autoSearch: false}
	if got := off.autoSearchPrompt(context.Background(), "u1", "明天上海天气怎么样", ag); got != "" {
		t.Fatalf("disabled auto search must not inject: %q", got)
	}
}

func TestAutoSearchPromptSkipsNonCleanAgent(t *testing.T) {
	searcher := &fakeSearcher{results: []agent.SearchResult{{Title: "x", Content: "y"}}}
	h := &Handler{webSearch: searcher, autoSearch: true}
	if got := h.autoSearchPrompt(context.Background(), "u1", "明天上海天气怎么样", &fakeAgent{}); got != "" {
		t.Fatalf("non-clean agent must skip: %q", got)
	}
	if len(searcher.queries) != 0 {
		t.Fatalf("non-clean agent must not search: %#v", searcher.queries)
	}
}

func TestAutoSearchPromptDropsIrrelevantDigest(t *testing.T) {
	searcher := &fakeSearcher{answer: "与问题无关", results: []agent.SearchResult{{Title: "广告", Content: "与问题无关"}}}
	h := &Handler{webSearch: searcher, autoSearch: true}
	ag := &fakeCleanAgent{replies: []string{`{"search":true,"query":"测试"}`, "无"}}
	if got := h.autoSearchPrompt(context.Background(), "u1", "这句话需要查点什么吗", ag); got != "" {
		t.Fatalf("irrelevant digest must not be injected: %q", got)
	}
}

func TestAutoSearchPromptSwallowsSearchErrors(t *testing.T) {
	searcher := &fakeSearcher{err: errors.New("boom")}
	h := &Handler{webSearch: searcher, autoSearch: true}
	ag := &fakeCleanAgent{replies: []string{`{"search":true,"query":"测试"}`}}
	if got := h.autoSearchPrompt(context.Background(), "u1", "这句话需要查点什么吗", ag); got != "" {
		t.Fatalf("search errors must degrade quietly: %q", got)
	}
}
