package messaging

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/fastclaw-ai/weclaw/agent"
	"github.com/fastclaw-ai/weclaw/ilink"
)

const searchDeadline = 90 * time.Second

var searchCmdPrefixes = []string{"/s", "/搜", "/search"}

// 只用明确的"要联网查"措辞；"查查这个方案"这类闲聊不在此列。
var searchTriggers = []string{
	"搜一下", "搜一搜", "搜索一下", "搜索", "帮我搜", "帮我查", "查一下",
	"search for", "look up",
}

// detectSearchRequest reports whether this message asks for a web search and
// returns the query with the trigger wording removed.
func detectSearchRequest(text string) (string, bool) {
	t := strings.TrimSpace(text)
	if t == "" {
		return "", false
	}
	low := strings.ToLower(t)
	for _, p := range searchCmdPrefixes {
		lp := strings.ToLower(p)
		if low == lp {
			return "", true
		}
		if strings.HasPrefix(low, lp+" ") {
			return cleanPrompt(t[len(lp):]), true
		}
	}
	if len([]rune(t)) > 120 {
		return "", false
	}
	lower := strings.ToLower(t)
	for _, tr := range searchTriggers {
		if i := strings.Index(lower, tr); i >= 0 {
			rest := strings.TrimPrefix(cleanPrompt(t[i+len(tr):]), "一下")
			rest = cleanPrompt(rest)
			if rest == "" {
				rest = cleanPrompt(t[:i])
			}
			if rest != "" {
				return rest, true
			}
		}
	}
	return "", false
}

// chineseSearchSummary asks the current default model to turn the search
// bundle (provider answer, snippets and fetched page text) into one Chinese
// summary. Returns "" when no agent is ready or the call fails, so the
// caller can fall back to the raw answer.
func (h *Handler) chineseSearchSummary(ctx context.Context, query string, bundle agent.DeepSearchBundle) string {
	ag := h.getDefaultAgent()
	if ag == nil {
		return ""
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "查询：%s\n", query)
	if bundle.Answer != "" {
		fmt.Fprintf(&sb, "原始摘要：%s\n", bundle.Answer)
	}
	for i, r := range bundle.Results {
		if i >= 5 {
			break
		}
		title := strings.TrimSpace(r.Title)
		snippet := strings.TrimSpace(r.Content)
		if utf8.RuneCountInString(snippet) > 180 {
			snippet = string([]rune(snippet)[:180]) + "…"
		}
		if snippet != "" {
			fmt.Fprintf(&sb, "- %s：%s\n", title, snippet)
		} else {
			fmt.Fprintf(&sb, "- %s\n", title)
		}
	}
	pages := 0
	for _, r := range bundle.Results {
		if pages >= 2 {
			break
		}
		text := strings.TrimSpace(bundle.Pages[r.URL])
		if text == "" {
			continue
		}
		if utf8.RuneCountInString(text) > 900 {
			text = string([]rune(text)[:900]) + "…"
		}
		fmt.Fprintf(&sb, "〔网页正文（%s）〕%s\n", r.Title, text)
		pages++
	}
	prompt := "把下面的联网搜索结果和网页正文整理成一段中文内容，不超过400字，只讲事实，不要编造，不要用Markdown和列表，直接输出正文。保留关键数字、时间和人名。涉及价格时统一换算成人民币并用“元”表述，外币按近期汇率约算，不要只给港元、美元等外币数字：\n\n" + sb.String()
	sctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	var out string
	var err error
	if cc, ok := ag.(agent.CleanChatter); ok {
		out, err = cc.ChatClean(sctx, prompt)
	} else {
		// Fixed pseudo-session: reset it so summaries never accumulate history.
		_, _ = ag.ResetSession(sctx, "search-summary")
		out, err = ag.Chat(sctx, "search-summary", prompt)
	}
	if err != nil {
		log.Printf("[search] summary failed: %v", err)
		return ""
	}
	out = strings.TrimSpace(MarkdownToPlainText(out))
	if utf8.RuneCountInString(out) > 500 {
		out = string([]rune(out)[:500])
	}
	return out
}

func (h *Handler) handleSearchRequest(ctx context.Context, client *ilink.Client, msg ilink.WeixinMessage, query, clientID string) {
	if h.webSearch == nil {
		SendTextReply(ctx, client, msg.FromUserID, "联网搜索没启用。", msg.ContextToken, clientID)
		return
	}
	if query == "" {
		SendTextReply(ctx, client, msg.FromUserID,
			"搜什么？直接说 搜一下 今天的天气，或者 /s 关键词", msg.ContextToken, clientID)
		return
	}
	_ = SendTypingState(ctx, client, msg.FromUserID, msg.ContextToken)
	sctx, cancel := context.WithTimeout(ctx, searchDeadline)
	defer cancel()
	start := time.Now()
	bundle, err := agent.DeepSearch(sctx, h.webSearch, []string{query}, 4)
	if err != nil {
		log.Printf("[search] failed: %v", err)
		SendTextReply(ctx, client, msg.FromUserID, "这次没搜成："+err.Error(), msg.ContextToken, clientID)
		return
	}
	log.Printf("[search] /s %q via %s: results=%d pages=%d", query, h.webSearch.Name(), len(bundle.Results), len(bundle.Pages))
	answer := bundle.Answer
	if summary := h.chineseSearchSummary(sctx, query, bundle); summary != "" {
		answer = summary
	} else if answer == "" {
		answer = fallbackSearchAnswer(bundle.Results)
	}
	SendTextReply(ctx, client, msg.FromUserID,
		buildSearchReply(query, answer, bundle.Results, h.webSearch.Name(), time.Since(start)), msg.ContextToken, clientID)
}

// fallbackSearchAnswer keeps /s useful when the summary model times out:
// join the first few result snippets instead of returning an empty reply.
func fallbackSearchAnswer(results []agent.SearchResult) string {
	var parts []string
	for i, r := range results {
		if i >= 3 {
			break
		}
		s := strings.TrimSpace(r.Content)
		if s == "" {
			continue
		}
		if utf8.RuneCountInString(s) > 140 {
			s = string([]rune(s)[:140]) + "…"
		}
		parts = append(parts, s)
	}
	out := strings.Join(parts, "；")
	if utf8.RuneCountInString(out) > 350 {
		out = string([]rune(out)[:350]) + "…"
	}
	return out
}

// buildSearchReply deliberately carries no URLs: WeChat bot text does not
// turn them into clickable links, so a link list would only be noise.
func buildSearchReply(query, answer string, results []agent.SearchResult, provider string, elapsed time.Duration) string {
	lines := []string{fmt.Sprintf("查询：%s", query)}
	if answer != "" {
		lines = append(lines, answer)
	}
	if len(results) == 0 && answer == "" {
		lines = append(lines, "没有搜到结果。")
	}
	lines = append(lines, fmt.Sprintf("%s · %.1fs", provider, elapsed.Seconds()))
	return modJoin(lines)
}
