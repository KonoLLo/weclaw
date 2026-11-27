package messaging

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/fastclaw-ai/weclaw/agent"
)

const (
	autoSearchTimeout    = 30 * time.Second
	autoSearchMinRunes   = 4
	autoSearchMaxQuery   = 80
	autoSearchMaxQueries = agent.DeepSearchMaxQueries
	autoSearchFetchPages = agent.DeepSearchDefaultPages
	autoSearchMaxDigest  = 700
	autoSearchMaxSnippet = 200
	autoSearchMaxPage    = 1100
)

// autoSearchPrompt decides whether this normal chat turn needs fresh facts,
// searches when it does, and returns a distilled context block to prepend.
// "" means the turn goes out without web material.
func (h *Handler) autoSearchPrompt(ctx context.Context, userID, text string, ag agent.Agent) string {
	if !h.autoSearch || h.webSearch == nil {
		return ""
	}
	cc, ok := ag.(agent.CleanChatter)
	if !ok {
		return ""
	}
	question := strings.TrimSpace(text)
	if utf8.RuneCountInString(question) < autoSearchMinRunes {
		return ""
	}
	sctx, cancel := context.WithTimeout(ctx, autoSearchTimeout)
	defer cancel()

	queries, need := autoSearchQueries(sctx, cc, question)
	if !need {
		return ""
	}
	bundle, err := agent.DeepSearch(sctx, h.webSearch, queries, autoSearchFetchPages)
	if err != nil {
		log.Printf("[search] auto search %q failed: %v", queries, err)
		return ""
	}
	if len(bundle.Results) == 0 && bundle.Answer == "" {
		log.Printf("[search] auto search %q returned nothing", queries)
		return ""
	}
	digest := autoSearchDigest(sctx, cc, question, bundle)
	if digest == "" {
		return ""
	}
	log.Printf("[search] auto context for %s (queries=%q, results=%d, pages=%d, digest=%d chars)",
		userID, bundle.Queries, len(bundle.Results), len(bundle.Pages), utf8.RuneCountInString(digest))
	return "【联网查到的资料（仅供参考；回答时不要提及搜索过程，也不要输出链接）】\n" + digest
}

// autoSearchQueries is the cheap router: it asks the model whether external
// facts are needed and, if so, for up to three complementary queries.
func autoSearchQueries(ctx context.Context, cc agent.CleanChatter, question string) ([]string, bool) {
	prompt := `你是联网搜索调度器。判断用户这句话要不要联网查实时或外部事实（新闻、价格、天气、赛事、人物近况、产品参数、教程、政策、事实核查等）才能答好。
只输出一行 JSON，不要解释：{"search":true,"queries":["检索词1","检索词2"]} 或 {"search":false}
规则：寒暄、情绪、闲聊、角色扮演、创作、数学计算、关于用户自己的信息、对聊天上下文的追问 → false。
需要多个角度时才给 1-3 个互补的短查询（中文关键词，不要引号）；一句话能问清就只给 1 个。
<消息>
` + question + `
</消息>`
	out, err := cc.ChatClean(ctx, prompt)
	if err != nil {
		log.Printf("[search] auto router failed: %v", err)
		return nil, false
	}
	return parseAutoSearchDecision(out)
}

func parseAutoSearchDecision(raw string) ([]string, bool) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return nil, false
	}
	if start, end := strings.Index(s, "{"), strings.LastIndex(s, "}"); start >= 0 && end > start {
		s = s[start : end+1]
	}
	var d struct {
		Search  bool     `json:"search"`
		Query   string   `json:"query"`
		Queries []string `json:"queries"`
	}
	if err := json.Unmarshal([]byte(s), &d); err != nil {
		return nil, false
	}
	if !d.Search {
		return nil, false
	}
	candidates := append([]string{d.Query}, d.Queries...)
	seen := make(map[string]bool)
	var out []string
	for _, q := range candidates {
		q = strings.TrimSpace(q)
		if q == "" || seen[strings.ToLower(q)] {
			continue
		}
		seen[strings.ToLower(q)] = true
		if utf8.RuneCountInString(q) > autoSearchMaxQuery {
			q = string([]rune(q)[:autoSearchMaxQuery])
		}
		out = append(out, q)
		if len(out) >= autoSearchMaxQueries {
			break
		}
	}
	if len(out) == 0 {
		return nil, false
	}
	return out, true
}

// autoSearchDigest condenses search snippets plus fetched page text into a
// few factual paragraphs. "无" (or failure) means the material was
// irrelevant and must not be sent.
func autoSearchDigest(ctx context.Context, cc agent.CleanChatter, question string, bundle agent.DeepSearchBundle) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "用户问题：%s\n", question)
	if strings.TrimSpace(bundle.Answer) != "" {
		fmt.Fprintf(&sb, "搜索摘要：%s\n", truncateRunes(strings.TrimSpace(bundle.Answer), 600))
	}
	pageBudget := 5500
	for i, r := range bundle.Results {
		if i >= 6 || pageBudget <= 0 {
			break
		}
		title := strings.TrimSpace(r.Title)
		snippet := truncateRunes(strings.TrimSpace(r.Content), autoSearchMaxSnippet)
		if title != "" || snippet != "" {
			fmt.Fprintf(&sb, "- %s：%s\n", title, snippet)
		}
		text := strings.TrimSpace(bundle.Pages[r.URL])
		if text == "" {
			continue
		}
		text = truncateRunes(text, autoSearchMaxPage)
		if utf8.RuneCountInString(text) > pageBudget {
			text = truncateRunes(text, pageBudget)
		}
		pageBudget -= utf8.RuneCountInString(text)
		fmt.Fprintf(&sb, "  〔网页正文（%s）〕%s\n", title, text)
	}
	prompt := `根据下面的搜索结果和网页正文，提炼与用户问题直接相关的客观事实，中文，700字以内。
保留关键数字、时间、地点、人名；不要编造，不要列表，不要链接，不要提及搜索过程；如果材料与问题无关或没有可用信息，只输出“无”。
` + sb.String()
	out, err := cc.ChatClean(ctx, prompt)
	if err != nil {
		log.Printf("[search] auto digest failed: %v", err)
		return ""
	}
	out = strings.TrimSpace(MarkdownToPlainText(out))
	if out == "" || strings.Trim(out, "。.!！ \n") == "无" {
		return ""
	}
	return truncateRunes(out, autoSearchMaxDigest)
}

func truncateRunes(s string, max int) string {
	if max <= 0 {
		return s
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}
