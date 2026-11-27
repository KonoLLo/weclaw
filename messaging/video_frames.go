package messaging

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/fastclaw-ai/weclaw/agent"
)

const (
	videoFrameMax      = 6
	videoFrameMaxWidth = 960
	videoFrameTimeout  = 45 * time.Second
)

var videoDurationPattern = regexp.MustCompile(`Duration:\s*(\d+):(\d+):(\d+(?:\.\d+)?)`)

// probeVideoDuration reads the duration from ffmpeg's banner. The VPS has no
// ffprobe, but `ffmpeg -i` prints the same summary and exits non-zero without
// an output file; that exit is expected here.
func probeVideoDuration(ctx context.Context, videoPath string) (time.Duration, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	out, _ := exec.CommandContext(ctx, "ffmpeg", "-hide_banner", "-i", videoPath).CombinedOutput()
	if ctx.Err() != nil {
		return 0, ctx.Err()
	}
	return parseFfmpegDuration(string(out))
}

func parseFfmpegDuration(raw string) (time.Duration, error) {
	m := videoDurationPattern.FindStringSubmatch(raw)
	if m == nil {
		return 0, fmt.Errorf("读不出视频时长")
	}
	hours, _ := strconv.Atoi(m[1])
	minutes, _ := strconv.Atoi(m[2])
	seconds, _ := strconv.ParseFloat(m[3], 64)
	d := time.Duration(hours)*time.Hour + time.Duration(minutes)*time.Minute + time.Duration(seconds*float64(time.Second))
	if d <= 0 {
		return 0, fmt.Errorf("视频时长为 0")
	}
	return d, nil
}

// videoFrameTimes spreads up to max frames between 5% and 92% of the clip,
// skipping the usually black first and last moments.
func videoFrameTimes(d time.Duration, max int) []time.Duration {
	if max <= 0 {
		max = videoFrameMax
	}
	sec := d.Seconds()
	n := max
	switch {
	case sec >= 12:
		n = 6
	case sec >= 6:
		n = 5
	case sec >= 3:
		n = 4
	default:
		n = 2
	}
	if n > max {
		n = max
	}
	times := make([]time.Duration, 0, n)
	for i := 0; i < n; i++ {
		frac := 0.05 + 0.87*float64(i)/float64(n-1)
		times = append(times, time.Duration(frac*sec*float64(time.Second)))
	}
	return times
}

func formatFrameTimes(times []time.Duration) string {
	parts := make([]string, 0, len(times))
	for _, t := range times {
		parts = append(parts, fmt.Sprintf("%.1fs", t.Seconds()))
	}
	return strings.Join(parts, ", ")
}

// extractVideoFrames writes up to maxFrames JPEGs to a temp dir and returns
// them as image attachments the HTTP models can actually see.
func extractVideoFrames(ctx context.Context, videoPath string, maxFrames int) ([]agent.Attachment, time.Duration, []time.Duration, error) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return nil, 0, nil, fmt.Errorf("服务器没装 ffmpeg")
	}
	duration, err := probeVideoDuration(ctx, videoPath)
	if err != nil {
		return nil, 0, nil, err
	}
	dir, err := os.MkdirTemp("", "weclaw-frames-*")
	if err != nil {
		return nil, 0, nil, err
	}
	times := videoFrameTimes(duration, maxFrames)
	frames := make([]agent.Attachment, 0, len(times))
	for i, t := range times {
		out := filepath.Join(dir, fmt.Sprintf("frame_%02d.jpg", i+1))
		args := []string{
			"-hide_banner", "-loglevel", "error", "-y",
			"-ss", strconv.FormatFloat(t.Seconds(), 'f', 2, 64),
			"-i", videoPath,
			"-frames:v", "1",
			"-vf", fmt.Sprintf("scale='min(%d,iw)':-2", videoFrameMaxWidth),
			"-q:v", "4",
			out,
		}
		if raw, err := exec.CommandContext(ctx, "ffmpeg", args...).CombinedOutput(); err != nil {
			os.RemoveAll(dir)
			return nil, duration, nil, fmt.Errorf("抽帧失败：%v：%s", err, strings.TrimSpace(string(raw)))
		}
		frames = append(frames, agent.Attachment{
			Path:     out,
			MIMEType: "image/jpeg",
			Name:     fmt.Sprintf("video_frame_%02d.jpg", i+1),
		})
	}
	return frames, duration, times, nil
}
