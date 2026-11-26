package agent

import "testing"

func TestNormalizeImageSize(t *testing.T) {
	cases := map[string]string{
		"1024x1024": "1024x1024",
		"1600x900":  "1344x768", // 16:9 更接近 1.75:1 而不是 2:1
		"1440x720":  "1440x720",
		"720×1440":  "720x1440",
		"800x1400":  "768x1344",
		"1200x800":  "1152x864",
		"900x1200":  "864x1152",
		"1600x1200": "1152x864",
		"2000x500":  "1440x720",
		"garbage":   "",
		"0x0":       "",
	}
	for in, want := range cases {
		if got := NormalizeImageSize(in); got != want {
			t.Errorf("%s -> %s, want %s", in, got, want)
		} else {
			t.Logf("%-10s -> %s", in, got)
		}
	}
}
