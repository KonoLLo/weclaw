package messaging

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const ffmpegBinary = "ffmpeg"

func ffmpegPath() (string, bool) {
	if p, err := exec.LookPath(ffmpegBinary); err == nil {
		return p, true
	}
	if _, err := os.Stat("/usr/local/bin/ffmpeg"); err == nil {
		return "/usr/local/bin/ffmpeg", true
	}
	return "", false
}

func FFmpegAvailable() bool {
	_, ok := ffmpegPath()
	return ok
}

// AudioToVideo wraps an mp3 in a tiny mp4 with a static cover so WeChat shows
// a playable video bubble; native voice items are silently dropped by iLink.
func AudioToVideo(ctx context.Context, mp3Path, outPath string) error {
	bin, ok := ffmpegPath()
	if !ok {
		return fmt.Errorf("ffmpeg 未安装")
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 60*time.Second)
		defer cancel()
	}
	if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
		return err
	}
	tmp := outPath + ".tmp.mp4"
	_ = os.Remove(tmp)
	args := []string{
		"-y", "-loglevel", "error",
		"-f", "lavfi", "-i", "color=c=0x16202c:s=640x640:r=10",
		"-i", mp3Path,
		"-c:v", "libx264", "-preset", "veryfast", "-tune", "stillimage",
		"-pix_fmt", "yuv420p",
		"-c:a", "aac", "-b:a", "96k", "-ar", "44100", "-ac", "2",
		"-shortest", "-movflags", "+faststart",
		tmp,
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		_ = os.Remove(tmp)
		msg := strings.TrimSpace(string(out))
		if r := []rune(msg); len(r) > 200 {
			msg = string(r[len(r)-200:])
		}
		return fmt.Errorf("ffmpeg 失败：%s", msg)
	}
	st, err := os.Stat(tmp)
	if err != nil || st.Size() < 1024 {
		_ = os.Remove(tmp)
		return fmt.Errorf("ffmpeg 输出异常")
	}
	return os.Rename(tmp, outPath)
}

func LastVoiceVideoPath() string {
	return filepath.Join(lastVoiceDir(), "voice.mp4")
}
