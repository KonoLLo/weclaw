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

const videoGenDeadline = 5*time.Minute + 30*time.Second

// videoCmdPrefixes force a generation call regardless of wording.
var videoCmdPrefixes = []string{"/视频", "/video"}

// 生成一段视频 / 做个视频 / 拍个视频 / 来段视频
var videoNounRe = regexp.MustCompile(`(?:帮我|给我|请|麻烦)?(?:生成|做|拍|来|制作|剪)(?:一|1|两|2|几)?[段个条]?视频`)

// English keeps the whole sentence: "generate a video of a fox" is already a
// usable prompt. Only explicit video wording triggers it.
var engVideoRe = regexp.MustCompile(`(?i)\b(?:generate|create|make|produce)\b[^.!?\n]{0,40}\bvideo\b`)

// detectVideoRequest reports whether this message asks for a video, and
// returns the prompt with the trigger wording stripped off.
func detectVideoRequest(text string) (prompt string, ok bool) {
	t := strings.TrimSpace(text)
	if t == "" {
		return "", false
	}
	low := strings.ToLower(t)
	for _, p := range videoCmdPrefixes {
		lp := strings.ToLower(p)
		if low == lp {
			return "", true
		}
		if strings.HasPrefix(low, lp+" ") {
			return cleanPrompt(t[len(lp):]), true
		}
	}
	// Long prose is chat, not a prompt.
	if len([]rune(t)) > 120 {
		return "", false
	}
	// "生成视频功能上线了吗" is a question, not an order.
	if strings.HasSuffix(t, "吗") || strings.HasSuffix(t, "呢") || strings.HasSuffix(t, "？") || strings.HasSuffix(t, "?") {
		return "", false
	}
	if loc := videoNounRe.FindStringIndex(t); loc != nil {
		if rest := cleanPrompt(t[loc[1]:]); rest != "" && !isQuestionParticle(rest) {
			return rest, true
		}
		if before := cleanPrompt(t[:loc[0]]); before != "" && before != "能" && before != "可以" {
			return before, true
		}
		return "", true
	}
	eng := strings.TrimSpace(engPoliteRe.ReplaceAllString(t, ""))
	if engVideoRe.MatchString(eng) {
		return eng, true
	}
	return "", false
}

func isQuestionParticle(s string) bool {
	switch strings.TrimSpace(s) {
	case "吗", "么", "呢", "嘛":
		return true
	}
	return false
}

// extractVideoSize pulls "1920x1080"-style hints out of the text and snaps
// them to a documented CogVideoX size.
func extractVideoSize(text string) (size, rest string) {
	loc := imageSizeRe.FindStringSubmatchIndex(text)
	if loc == nil {
		return "", text
	}
	size = agent.NormalizeVideoSize(text[loc[2]:loc[3]] + "x" + text[loc[4]:loc[5]])
	rest = cleanPrompt(strings.TrimSpace(text[:loc[0]] + " " + text[loc[1]:]))
	return size, rest
}

func (h *Handler) handleVideoRequest(ctx context.Context, client *ilink.Client, msg ilink.WeixinMessage, prompt, size, clientID string) {
	if h.videoGen == nil {
		return
	}
	if prompt == "" {
		SendTextReply(ctx, client, msg.FromUserID,
			"要生成什么视频？直接说 生成一段海边日落的视频，或者 /视频 海边日落，也可以带尺寸 1920x1080",
			msg.ContextToken, clientID)
		return
	}
	if size == "" {
		size = h.videoGen.Size()
	}
	if err := SendTextReply(ctx, client, msg.FromUserID,
		"视频开始生成，免费模型一般 1-2 分钟，有结果就直接发你。", msg.ContextToken, clientID); err != nil {
		log.Printf("[video] ack failed: %v", err)
	}

	taskCtx, finish := h.beginTask(ctx, msg.FromUserID, "生成视频", nil)
	defer finish()
	vctx, cancel := context.WithTimeout(taskCtx, videoGenDeadline)
	defer cancel()
	start := time.Now()
	url, err := h.videoGen.GenerateVideo(vctx, prompt, size)
	if taskCtx.Err() != nil {
		log.Printf("[video] generation canceled for %s", msg.FromUserID)
		return
	}
	if err != nil {
		log.Printf("[video] generate failed: %v", err)
		reply := "这次视频没生成出来：" + err.Error()
		if strings.Contains(err.Error(), "1301") || strings.Contains(err.Error(), "敏感") {
			reply = "这次被平台内容审核拦了，换个描述再试一次（动作写温和些，比如“橘猫在草地上看蝴蝶”）。"
		}
		SendTextReply(ctx, client, msg.FromUserID, reply, msg.ContextToken, clientID)
		return
	}
	if err := SendMediaFromURL(ctx, client, msg.FromUserID, url, msg.ContextToken); err != nil {
		log.Printf("[video] send failed: %v", err)
		SendTextReply(ctx, client, msg.FromUserID,
			"视频是生成好了，但发不出去："+err.Error()+"\n\n地址（会过期，要留就早点存）：\n\n"+url,
			msg.ContextToken, clientID)
		return
	}
	SendTextReply(ctx, client, msg.FromUserID,
		fmt.Sprintf("%s · %s · %s · %.0fs", h.videoGen.Model(), size, prompt, time.Since(start).Seconds()),
		msg.ContextToken, clientID)
}
