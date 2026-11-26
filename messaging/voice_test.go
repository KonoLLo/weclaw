package messaging

import (
	"strings"
	"testing"

	"github.com/fastclaw-ai/weclaw/agent"
)

func TestSplitVoiceArg(t *testing.T) {
	h := &Handler{tts: agent.NewTTS("")}
	cases := []struct{ in, voice, text string }{
		{"你好呀", "", "你好呀"},
		{"云希 你好呀", "云希", "你好呀"},
		{"晓晓", "", "晓晓"},
		{"zh-CN-YunxiNeural hello", "zh-CN-YunxiNeural", "hello"},
		{"男声 讲个笑话", "男声", "讲个笑话"},
	}
	for _, c := range cases {
		v, txt := h.splitVoiceArg(c.in)
		if v != c.voice || txt != c.text {
			t.Errorf("splitVoiceArg(%q) = %q/%q, want %q/%q", c.in, v, txt, c.voice, c.text)
		}
	}
}

func TestCleanSpeechText(t *testing.T) {
	in := "**晚上好**\n\n今天想听我唱什么？\n\n「月亮爬上来」"
	got := cleanSpeechText(in)
	if strings.Contains(got, "*") || strings.Contains(got, "\n") {
		t.Fatalf("没清理干净：%q", got)
	}
	if !strings.Contains(got, "月亮爬上来") {
		t.Fatalf("内容丢了：%q", got)
	}
}