package messaging

import (
	"context"
	"fmt"
	"log"
	"regexp"
	"strings"
	"time"

	"github.com/fastclaw-ai/weclaw/agent"
	"github.com/fastclaw-ai/weclaw/ilink"
)

const imageGenDeadline = 150 * time.Second

// imageCmdPrefixes force a drawing call regardless of wording.
var imageCmdPrefixes = []string{"/图", "/img", "/image"}

// 1440x720 / 1440×720 / 1440*720, anywhere in the message.
var imageSizeRe = regexp.MustCompile(`(?i)(\d{3,4})\s*[x×*]\s*(\d{3,4})`)

// extractImageSize pulls "1600x900"-style hints out of the text and snaps them
// to a documented CogView size.
func extractImageSize(text string) (size, rest string) {
	loc := imageSizeRe.FindStringSubmatchIndex(text)
	if loc == nil {
		return "", text
	}
	size = agent.NormalizeImageSize(text[loc[2]:loc[3]] + "x" + text[loc[4]:loc[5]])
	rest = cleanPrompt(strings.TrimSpace(text[:loc[0]] + " " + text[loc[1]:]))
	return size, rest
}

// 画一张图 / 帮我生成图片 / 出图
var drawNounRe = regexp.MustCompile(`(?:帮我|给我|请|麻烦)?(?:画|生成|生|制作|绘制|出|来)(?:一|1|两|2|几)?[张幅个]?图(?:片)?`)

// 画一只戴帽子的橘猫（没有"图"字，但动词开头就是要画）
var drawVerbLeadRe = regexp.MustCompile(`^(?:帮我|给我|请|麻烦)?(?:画|绘制)(?:一|1|两|2)?[张幅个只头尾]?`)

// English keeps the whole sentence: "generate an image of a fox" is already a
// usable prompt once the request wording is stripped.
var engPoliteRe = regexp.MustCompile(`(?i)^(?:please |can you |could you |hey |i want you to )+`)
var engNounRe = regexp.MustCompile(`(?i)\b(?:draw|generate|create|make)\b[^.!?\n]{0,40}\b(?:image|picture|pic|illustration|avatar|wallpaper)\b`)
var engVerbLeadRe = regexp.MustCompile(`(?i)^(?:draw|sketch|illustrate)\s+`)
var engIndirectRe = regexp.MustCompile(`(?i)^(?:me|us)\s+`)

// detectImageRequest reports whether this message is a drawing request, and
// returns the prompt with the trigger wording stripped off.
func detectImageRequest(text string) (prompt string, ok bool) {
	t := strings.TrimSpace(text)
	if t == "" {
		return "", false
	}
	low := strings.ToLower(t)
	for _, p := range imageCmdPrefixes {
		lp := strings.ToLower(p)
		if low == lp {
			return "", true
		}
		if strings.HasPrefix(low, lp+" ") {
			return strings.TrimSpace(t[len(lp):]), true
		}
	}
	// Long prose is chat, not a prompt.
	if len([]rune(t)) > 120 {
		return "", false
	}
	if loc := drawNounRe.FindStringIndex(t); loc != nil {
		if rest := strings.TrimSpace(t[loc[1]:]); rest != "" {
			return cleanPrompt(rest), true
		}
		if before := strings.TrimSpace(t[:loc[0]]); before != "" {
			return cleanPrompt(before), true
		}
		return "", true
	}
	eng := strings.TrimSpace(engPoliteRe.ReplaceAllString(t, ""))
	if engVerbLeadRe.MatchString(eng) {
		rest := strings.TrimSpace(engIndirectRe.ReplaceAllString(strings.TrimSpace(engVerbLeadRe.ReplaceAllString(eng, "")), ""))
		if rest != "" {
			return rest, true
		}
		return eng, true
	}
	if engNounRe.MatchString(eng) {
		return eng, true
	}
	if loc := drawVerbLeadRe.FindStringIndex(t); loc != nil && loc[0] == 0 {
		if rest := strings.TrimSpace(t[loc[1]:]); rest != "" {
			return cleanPrompt(rest), true
		}
	}
	return "", false
}

// textWithoutSize keeps the size hint from confusing the trigger regexes.
func textWithoutSize(text string) string {
	if _, rest := extractImageSize(text); rest != text {
		return rest
	}
	return text
}

func cleanPrompt(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimLeft(s, "，,。.:：;；、 ")
	return strings.Join(strings.Fields(s), " ")
}

func (h *Handler) handleImageRequest(ctx context.Context, client *ilink.Client, msg ilink.WeixinMessage, prompt, size, clientID string) {
	if h.imageGen == nil {
		return
	}
	if prompt == "" {
		SendTextReply(ctx, client, msg.FromUserID,
			"要画什么？直接说 画一只戴帽子的橘猫，或者 /图 一只戴帽子的橘猫",
			msg.ContextToken, clientID)
		return
	}
	if size == "" {
		size = h.imageGen.Size()
	}
	taskCtx, finish := h.beginTask(ctx, msg.FromUserID, "生图", nil)
	defer finish()
	gctx, cancel := context.WithTimeout(taskCtx, imageGenDeadline)
	defer cancel()
	start := time.Now()
	url, err := h.imageGen.GenerateImage(gctx, prompt, size)
	if taskCtx.Err() != nil {
		log.Printf("[image] generation canceled for %s", msg.FromUserID)
		return
	}
	if err != nil {
		log.Printf("[image] generate failed: %v", err)
		SendTextReply(ctx, client, msg.FromUserID, "这次没画出来："+err.Error(), msg.ContextToken, clientID)
		return
	}
	if err := SendMediaFromURL(ctx, client, msg.FromUserID, url, msg.ContextToken); err != nil {
		log.Printf("[image] send failed: %v", err)
		SendTextReply(ctx, client, msg.FromUserID,
			"图是画好了，但发不出去："+err.Error()+"\n\n地址（会过期，要留就早点存）：\n\n"+url,
			msg.ContextToken, clientID)
		return
	}
	SendTextReply(ctx, client, msg.FromUserID,
		fmt.Sprintf("%s · %s · %s · %.0fs", h.imageGen.Model(), size, prompt, time.Since(start).Seconds()),
		msg.ContextToken, clientID)
}
