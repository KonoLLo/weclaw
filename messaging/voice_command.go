package messaging

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/fastclaw-ai/weclaw/ilink"
)

var voiceCmdPrefixes = []string{"/语音", "/voice", "/说"}

// Phrases that ask for the *next* reply to be spoken.
var voiceSayPrefixes = []string{
	"用语音说", "用语音念", "用语音讲", "用语音回复", "语音回复我", "语音回复",
	"发条语音说", "发语音说", "语音说",
}

var voiceFileTriggers = []string{
	"把音频文件发我", "把音频文件发给我", "音频文件发我", "发音频文件",
	"把刚才的语音发我", "把语音文件发我", "我要音频文件", "发我音频文件",
}

var singTriggers = []string{"唱首歌", "唱一首", "唱个歌", "来首歌", "唱一段", "给我唱", "唱首"}

var voiceAliasLeadRe = regexp.MustCompile(`^用(.+?)的(?:声音|语气|音色)(?:说|讲|念|回复)?[：:，,\s]*`)

// handleVoiceCommand returns true when the message was fully handled here.
func (h *Handler) handleVoiceCommand(ctx context.Context, client *ilink.Client, msg ilink.WeixinMessage, trimmed, clientID string) bool {
	if h.tts == nil {
		return false
	}
	low := strings.ToLower(trimmed)

	if trimmed == "/音色" || trimmed == "/voices" {
		h.replyVoices(ctx, client, msg, clientID)
		return true
	}
	for _, t := range voiceFileTriggers {
		if strings.Contains(trimmed, t) {
			h.sendLastVoiceFile(ctx, client, msg, clientID)
			return true
		}
	}
	for _, p := range voiceCmdPrefixes {
		lp := strings.ToLower(p)
		if low == lp || strings.HasPrefix(low, lp+" ") || strings.HasPrefix(low, lp+"：") || strings.HasPrefix(low, lp+":") {
			arg := strings.TrimSpace(trimmed[len(p):])
			arg = strings.TrimLeft(arg, "：:，, ")
			voice, text := h.splitVoiceArg(arg)
			h.speakText(ctx, client, msg, text, voice, clientID)
			return true
		}
	}
	if m := voiceAliasLeadRe.FindStringSubmatch(trimmed); m != nil {
		rest := strings.TrimSpace(trimmed[len(m[0]):])
		if rest != "" && h.tts.IsKnownVoice(m[1]) {
			h.speakText(ctx, client, msg, rest, m[1], clientID)
			return true
		}
	}
	for _, p := range voiceSayPrefixes {
		if strings.HasPrefix(trimmed, p) {
			text := strings.TrimLeft(strings.TrimSpace(trimmed[len(p):]), "：:，, ")
			if text == "" {
				text = "好的，这条就是语音回复。"
			}
			h.speakText(ctx, client, msg, text, "", clientID)
			return true
		}
	}
	for _, t := range singTriggers {
		if strings.Contains(trimmed, t) {
			h.singSong(ctx, client, msg, clientID)
			return true
		}
	}
	return false
}

// splitVoiceArg pulls an optional voice alias off the front: "/语音 云希 你好".
func (h *Handler) splitVoiceArg(arg string) (voice, text string) {
	arg = strings.TrimSpace(arg)
	if arg == "" {
		return "", ""
	}
	fields := strings.Fields(arg)
	if len(fields) >= 2 && h.tts.IsKnownVoice(fields[0]) {
		return fields[0], strings.TrimSpace(arg[len(fields[0]):])
	}
	return "", arg
}

func (h *Handler) replyVoices(ctx context.Context, client *ilink.Client, msg ilink.WeixinMessage, clientID string) {
	lines := []string{"可选音色（默认晓晓）："}
	for _, v := range h.tts.Voices() {
		lines = append(lines, fmt.Sprintf("%s · %s", v.Alias, v.Desc))
	}
	lines = append(lines, "用法：/语音 云希 你好呀")
	lines = append(lines, "语音会以能直接点开听的视频气泡发你；要原始音频文件就说“把音频文件发我”。")
	_ = SendTextReply(ctx, client, msg.FromUserID, strings.Join(lines, "\n\n"), msg.ContextToken, clientID)
}

func (h *Handler) sendLastVoiceFile(ctx context.Context, client *ilink.Client, msg ilink.WeixinMessage, clientID string) {
	path := LastVoicePath()
	if _, err := os.Stat(path); err != nil {
		_ = SendTextReply(ctx, client, msg.FromUserID, "还没有生成过语音，先让我说一句或唱一首吧。", msg.ContextToken, clientID)
		return
	}
	if err := SendMediaFromPath(ctx, client, msg.FromUserID, path, msg.ContextToken); err != nil {
		log.Printf("[voice] send file failed: %v", err)
		_ = SendTextReply(ctx, client, msg.FromUserID, "音频文件发送失败："+err.Error(), msg.ContextToken, clientID)
		return
	}
	log.Printf("[voice] sent last clip as file to %s", msg.FromUserID)
}

func (h *Handler) speakText(ctx context.Context, client *ilink.Client, msg ilink.WeixinMessage, text, voice, clientID string) {
	text = cleanSpeechText(text)
	if text == "" {
		_ = SendTextReply(ctx, client, msg.FromUserID,
			"想让谁说什么？\n\n用法：/语音 你好呀\n\n换声音：/语音 云希 你好呀\n\n看音色：/音色",
			msg.ContextToken, clientID)
		return
	}
	start := time.Now()
	mp3, ms, used, err := h.tts.Synthesize(ctx, text, voice)
	if err != nil {
		log.Printf("[voice] synth failed: %v", err)
		_ = SendTextReply(ctx, client, msg.FromUserID, "语音合成失败："+err.Error(), msg.ContextToken, clientID)
		return
	}
	clipPath, err := SaveLastVoice(mp3, text)
	if err != nil {
		log.Printf("[voice] save clip failed: %v", err)
		_ = SendTextReply(ctx, client, msg.FromUserID, "音频保存失败："+err.Error(), msg.ContextToken, clientID)
		return
	}
	log.Printf("[voice] saved clip to %s", clipPath)

	sendPath := clipPath
	if FFmpegAvailable() {
		videoPath := LastVoiceVideoPath()
		vctx, vcancel := context.WithTimeout(ctx, 60*time.Second)
		err := AudioToVideo(vctx, clipPath, videoPath)
		vcancel()
		if err != nil {
			log.Printf("[voice] audio->video failed, falling back to mp3: %v", err)
		} else {
			sendPath = videoPath
		}
	}
	if err := SendMediaFromPath(ctx, client, msg.FromUserID, sendPath, msg.ContextToken); err != nil {
		log.Printf("[voice] send media failed: %v", err)
		_ = SendTextReply(ctx, client, msg.FromUserID, "语音发送失败："+err.Error(), msg.ContextToken, clientID)
		return
	}
	log.Printf("[voice] %s spoke %dms in %.1fs, sent %s: %q", used, ms, time.Since(start).Seconds(), filepath.Base(sendPath), truncate(text, 40))
}

func (h *Handler) singSong(ctx context.Context, client *ilink.Client, msg ilink.WeixinMessage, clientID string) {
	h.mu.RLock()
	name := h.defaultName
	h.mu.RUnlock()
	if name == "" {
		_ = SendTextReply(ctx, client, msg.FromUserID, "现在没有可用的对话模型，唱不了。", msg.ContextToken, clientID)
		return
	}
	ag, err := h.getAgent(ctx, name)
	if err != nil {
		_ = SendTextReply(ctx, client, msg.FromUserID, "模型没起来，唱不了："+err.Error(), msg.ContextToken, clientID)
		return
	}
	_ = SendTypingState(ctx, client, msg.FromUserID, msg.ContextToken)
	prompt := "用户想听你唱一小段歌。请只输出歌词本身：口语、轻快、不超过 80 字，不要解释、不要加引号、不要标点以外的格式。"
	lyrics, err := ag.Chat(ctx, msg.FromUserID, prompt)
	if err != nil {
		log.Printf("[voice] lyrics generation failed: %v", err)
		_ = SendTextReply(ctx, client, msg.FromUserID, "歌词没写出来："+err.Error(), msg.ContextToken, clientID)
		return
	}
	lyrics = cleanSpeechText(lyrics)
	if lyrics == "" {
		_ = SendTextReply(ctx, client, msg.FromUserID, "歌词是空的，再试一次？", msg.ContextToken, clientID)
		return
	}
	h.speakText(ctx, client, msg, lyrics, "晓伊", clientID)
}

func cleanSpeechText(s string) string {
	s = MarkdownToPlainText(s)
	s = strings.TrimSpace(s)
	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	for _, ln := range lines {
		if ln = strings.TrimSpace(ln); ln != "" {
			out = append(out, ln)
		}
	}
	s = strings.Join(out, "，")
	s = strings.NewReplacer(
		"**", "", "__", "", "`", "", "*", "", "#", "",
		"「", "", "」", "", "『", "", "』", "",
		"\"", "", "“", "", "”", "", "'", "",
	).Replace(s)
	for _, pair := range [][2]string{
		{"？，", "？"}, {"。，", "。"}, {"！，", "！"}, {"；，", "；"}, {"：，", "："},
		{"，，", "，"}, {"，。", "。"}, {"，！", "！"}, {"，？", "？"},
	} {
		s = strings.ReplaceAll(s, pair[0], pair[1])
	}
	if utf8.RuneCountInString(s) > 300 {
		s = string([]rune(s)[:300])
	}
	return strings.TrimSpace(s)
}
