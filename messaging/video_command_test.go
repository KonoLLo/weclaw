package messaging

import (
	"testing"

	"github.com/fastclaw-ai/weclaw/agent"
)

func TestDetectVideoRequest(t *testing.T) {
	cases := []struct {
		text   string
		prompt string
		ok     bool
	}{
		{"/视频", "", true},
		{"/视频 一只橘猫追蝴蝶", "一只橘猫追蝴蝶", true},
		{"/video a cat chasing a butterfly", "a cat chasing a butterfly", true},
		{"帮我生成一段视频：海边日落", "海边日落", true},
		{"做个视频 一只猫在跑", "一只猫在跑", true},
		{"拍个视频", "", true},
		{"generate a video of a fox running", "generate a video of a fox running", true},
		{"视频生成功能上线了吗", "", false},
		{"生成视频功能上线了吗", "", false},
		{"我想看个视频", "", false},
		{"这个视频真好看", "", false},
	}
	for _, c := range cases {
		prompt, ok := detectVideoRequest(c.text)
		if prompt != c.prompt || ok != c.ok {
			t.Errorf("detectVideoRequest(%q) = (%q,%v), want (%q,%v)", c.text, prompt, ok, c.prompt, c.ok)
		}
	}
}

func TestExtractVideoSize(t *testing.T) {
	size, rest := extractVideoSize("生成一段视频 1440x720 海边日落")
	if size != agent.NormalizeVideoSize("1440x720") {
		t.Errorf("size = %q", size)
	}
	if rest != "生成一段视频 海边日落" {
		t.Errorf("rest = %q", rest)
	}
	size, rest = extractVideoSize("1920×1080 一只猫")
	if size != "1920x1080" || rest != "一只猫" {
		t.Errorf("got (%q,%q)", size, rest)
	}
	size, rest = extractVideoSize("没有尺寸")
	if size != "" || rest != "没有尺寸" {
		t.Errorf("got (%q,%q)", size, rest)
	}
}
