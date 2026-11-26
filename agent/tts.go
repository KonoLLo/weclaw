package agent

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
	"unicode/utf8"
)

// Edge TTS is driven through the edge-tts CLI (Python) because the endpoint
// now requires a signed Sec-MS-GEC token that the CLI maintains upstream.
const (
	DefaultTTSVoice = "zh-CN-XiaoxiaoNeural"
	ttsMaxRunes     = 300
	ttsTimeout      = 60 * time.Second
	edgeTTSBinary   = "edge-tts"
)

type VoiceOption struct {
	Alias string
	Voice string
	Desc  string
}

var TTSVoices = []VoiceOption{
	{"晓晓", "zh-CN-XiaoxiaoNeural", "女声·温暖（默认）"},
	{"晓伊", "zh-CN-XiaoyiNeural", "女声·活泼"},
	{"云希", "zh-CN-YunxiNeural", "男声·阳光"},
	{"云扬", "zh-CN-YunyangNeural", "男声·新闻"},
	{"云健", "zh-CN-YunjianNeural", "男声·激情"},
	{"云夏", "zh-CN-YunxiaNeural", "男声·可爱"},
	{"小北", "zh-CN-liaoning-XiaobeiNeural", "女声·东北话"},
	{"小妮", "zh-CN-shaanxi-XiaoniNeural", "女声·陕西话"},
}

// Aliases that are not listed but are natural to say.
var ttsVoiceExtras = map[string]string{
	"女": "zh-CN-XiaoxiaoNeural", "女声": "zh-CN-XiaoxiaoNeural", "默认": "zh-CN-XiaoxiaoNeural",
	"男": "zh-CN-YunxiNeural", "男声": "zh-CN-YunxiNeural", "阳光": "zh-CN-YunxiNeural",
	"新闻": "zh-CN-YunyangNeural", "激情": "zh-CN-YunjianNeural",
	"可爱": "zh-CN-YunxiaNeural", "活泼": "zh-CN-XiaoyiNeural",
	"东北": "zh-CN-liaoning-XiaobeiNeural", "陕西": "zh-CN-shaanxi-XiaoniNeural",
}

type TTS struct {
	defaultVoice string
	binary       string
	found        bool
}

func NewTTS(voice string) *TTS {
	if strings.TrimSpace(voice) == "" {
		voice = DefaultTTSVoice
	}
	t := &TTS{defaultVoice: voice, binary: edgeTTSBinary}
	if p, err := exec.LookPath(edgeTTSBinary); err == nil {
		t.binary, t.found = p, true
	} else if _, statErr := os.Stat("/usr/local/bin/edge-tts"); statErr == nil {
		t.binary, t.found = "/usr/local/bin/edge-tts", true
	}
	return t
}

func (t *TTS) Available() bool { return t.found }

func (t *TTS) Voice() string { return t.defaultVoice }

func (t *TTS) Voices() []VoiceOption { return TTSVoices }

func (t *TTS) IsKnownVoice(name string) bool {
	n := strings.TrimSpace(name)
	if n == "" {
		return false
	}
	for _, v := range TTSVoices {
		if strings.EqualFold(n, v.Alias) || strings.EqualFold(n, v.Voice) {
			return true
		}
	}
	_, ok := ttsVoiceExtras[n]
	return ok
}

// ResolveVoice accepts an alias ("云希") or a full voice name; unknown input
// falls back to the default so a typo still produces audio.
func (t *TTS) ResolveVoice(name string) (voice, display string) {
	n := strings.TrimSpace(name)
	if n == "" {
		return t.defaultVoice, aliasOf(t.defaultVoice)
	}
	for _, v := range TTSVoices {
		if strings.EqualFold(n, v.Alias) || strings.EqualFold(n, v.Voice) {
			return v.Voice, v.Alias
		}
	}
	if v, ok := ttsVoiceExtras[n]; ok {
		return v, aliasOf(v)
	}
	return t.defaultVoice, aliasOf(t.defaultVoice)
}

func aliasOf(voice string) string {
	for _, v := range TTSVoices {
		if v.Voice == voice {
			return v.Alias
		}
	}
	return voice
}

// Synthesize returns mp3 bytes, playtime in ms, and which voice was used.
func (t *TTS) Synthesize(ctx context.Context, text, voice string) ([]byte, int, string, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, 0, "", fmt.Errorf("文本是空的")
	}
	if utf8.RuneCountInString(text) > ttsMaxRunes {
		text = string([]rune(text)[:ttsMaxRunes])
	}
	v, display := t.ResolveVoice(voice)

	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, ttsTimeout)
		defer cancel()
	}

	f, err := os.CreateTemp("", "weclaw-tts-*.mp3")
	if err != nil {
		return nil, 0, "", err
	}
	path := f.Name()
	_ = f.Close()
	defer os.Remove(path)

	cmd := exec.CommandContext(ctx, t.binary,
		"--voice", v,
		"--text", text,
		"--write-media", path,
	)
	cmd.Env = append(os.Environ(), "PYTHONIOENCODING=utf-8")
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if r := []rune(msg); len(r) > 200 {
			msg = string(r[len(r)-200:])
		}
		return nil, 0, display, fmt.Errorf("edge-tts 失败：%s", msg)
	}
	mp3, err := os.ReadFile(path)
	if err != nil {
		return nil, 0, display, err
	}
	if len(mp3) < 512 {
		return nil, 0, display, fmt.Errorf("edge-tts 只返回了 %d 字节", len(mp3))
	}
	ms := MP3DurationMs(mp3)
	if ms <= 0 {
		ms = 1000
	}
	return mp3, ms, display, nil
}

var mp3BitratesV1 = []int{0, 32, 40, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320}
var mp3BitratesV2 = []int{0, 8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160}
var mp3RatesV1 = []int{44100, 48000, 32000}
var mp3RatesV2 = []int{22050, 24000, 16000}
var mp3RatesV25 = []int{11025, 12000, 8000}

// MP3DurationMs walks frame headers; free TTS endpoints use varying bitrates,
// so byte size alone cannot give the playtime the voice item needs.
func MP3DurationMs(b []byte) int {
	if len(b) > 10 && string(b[:3]) == "ID3" {
		size := int(b[6])<<21 | int(b[7])<<14 | int(b[8])<<7 | int(b[9])
		if 10+size < len(b) {
			b = b[10+size:]
		}
	}
	totalSamples, sampleRate := 0, 0
	for i := 0; i+4 <= len(b); {
		if b[i] != 0xFF || b[i+1]&0xE0 != 0xE0 {
			i++
			continue
		}
		version := (b[i+1] >> 3) & 0x03
		layer := (b[i+1] >> 1) & 0x03
		if layer != 1 || version == 1 {
			i++
			continue
		}
		brIdx := int(b[i+2] >> 4)
		srIdx := int((b[i+2] >> 2) & 0x03)
		padding := int((b[i+2] >> 1) & 0x01)
		if brIdx == 0 || brIdx == 15 || srIdx == 3 {
			i++
			continue
		}
		var bitrate, rate, spf, coef int
		switch version {
		case 3:
			bitrate, rate, spf, coef = mp3BitratesV1[brIdx], mp3RatesV1[srIdx], 1152, 144
		case 2:
			bitrate, rate, spf, coef = mp3BitratesV2[brIdx], mp3RatesV2[srIdx], 576, 72
		default:
			bitrate, rate, spf, coef = mp3BitratesV2[brIdx], mp3RatesV25[srIdx], 576, 72
		}
		frameLen := coef*bitrate*1000/rate + padding
		if frameLen <= 4 || i+frameLen > len(b) {
			break
		}
		totalSamples += spf
		sampleRate = rate
		i += frameLen
	}
	if sampleRate == 0 {
		return 0
	}
	return totalSamples * 1000 / sampleRate
}
