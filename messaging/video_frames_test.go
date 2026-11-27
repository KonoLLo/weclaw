package messaging

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestParseFfmpegDuration(t *testing.T) {
	raw := `Input #0, mov,mp4,m4a,3gp,3g2,mj2, from 'v.mp4':
  Metadata:
    major_brand     : isom
  Duration: 00:00:44.87, start: 0.000000, bitrate: 539 kb/s
  Stream #0:0(und): Video: hevc`
	d, err := parseFfmpegDuration(raw)
	if err != nil {
		t.Fatal(err)
	}
	if d != 44*time.Second+870*time.Millisecond {
		t.Fatalf("unexpected duration: %s", d)
	}
	if _, err := parseFfmpegDuration("no duration here"); err == nil {
		t.Fatal("expected error for missing duration")
	}
}

func TestVideoFrameTimes(t *testing.T) {
	cases := []struct {
		duration time.Duration
		want     int
	}{
		{1 * time.Second, 2},
		{3 * time.Second, 4},
		{8 * time.Second, 5},
		{45 * time.Second, 6},
		{3 * time.Minute, 6},
	}
	for _, tc := range cases {
		times := videoFrameTimes(tc.duration, 6)
		if len(times) != tc.want {
			t.Fatalf("duration %s: got %d frames, want %d", tc.duration, len(times), tc.want)
		}
		for i, at := range times {
			if at <= 0 || at >= tc.duration {
				t.Fatalf("duration %s: frame %d at %s out of range", tc.duration, i, at)
			}
			if i > 0 && at <= times[i-1] {
				t.Fatalf("duration %s: frame times not increasing: %#v", tc.duration, times)
			}
		}
	}
}

func TestExtractVideoFramesWithFfmpeg(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	dir := t.TempDir()
	video := filepath.Join(dir, "sample.mp4")
	gen := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc=duration=3:size=320x240:rate=10",
		"-pix_fmt", "yuv420p", video)
	if out, err := gen.CombinedOutput(); err != nil {
		t.Skipf("cannot generate sample video: %v: %s", err, out)
	}

	frames, duration, times, err := extractVideoFrames(context.Background(), video, 6)
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(filepath.Dir(frames[0].Path))
	if len(frames) != len(times) || len(frames) < 2 {
		t.Fatalf("unexpected frames: %d times=%d", len(frames), len(times))
	}
	if duration < 2*time.Second || duration > 4*time.Second {
		t.Fatalf("unexpected duration: %s", duration)
	}
	for _, f := range frames {
		info, err := os.Stat(f.Path)
		if err != nil || info.Size() == 0 {
			t.Fatalf("frame %s missing or empty: %v", f.Path, err)
		}
		if f.MIMEType != "image/jpeg" {
			t.Fatalf("unexpected mime: %s", f.MIMEType)
		}
	}
}
