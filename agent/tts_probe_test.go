package agent

import (
	"context"
	"testing"
	"time"
)

func TestEdgeTTS(t *testing.T) {
	tts := NewTTS("")
	if !tts.Available() {
		t.Skip("edge-tts not installed")
	}
	start := time.Now()
	mp3, ms, used, err := tts.Synthesize(context.Background(), "你好，我是小肥鱼，这是一条微软语音测试。", "")
	if err != nil {
		t.Fatalf("失败（%.1fs）: %v", time.Since(start).Seconds(), err)
	}
	if len(mp3) < 512 || ms < 500 {
		t.Fatalf("结果异常：%d 字节 %dms", len(mp3), ms)
	}
	t.Logf("用时 %.2fs 字节 %d 时长 %dms 音色 %s 头 %x", time.Since(start).Seconds(), len(mp3), ms, used, mp3[:4])
}

func TestResolveVoice(t *testing.T) {
	tts := NewTTS("")
	cases := []struct{ in, voice, display string }{
		{"", DefaultTTSVoice, "晓晓"},
		{"云希", "zh-CN-YunxiNeural", "云希"},
		{"男声", "zh-CN-YunxiNeural", "云希"},
		{"zh-CN-YunyangNeural", "zh-CN-YunyangNeural", "云扬"},
		{"不存在的音色", DefaultTTSVoice, "晓晓"},
	}
	for _, c := range cases {
		v, d := tts.ResolveVoice(c.in)
		if v != c.voice || d != c.display {
			t.Errorf("ResolveVoice(%q) = %q/%q, want %q/%q", c.in, v, d, c.voice, c.display)
		}
	}
}