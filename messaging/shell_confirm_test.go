package messaging

import "testing"

func TestShellConfirmationVerdict(t *testing.T) {
	cases := map[string]string{
		"确认":        "run",
		"  确认执行 ":   "run",
		"Confirm":   "run",
		"取消":        "cancel",
		"算了":        "cancel",
		"确认一下这是啥命令": "",
		"确认。":       "",
		"好的，确认":     "",
	}
	for text, want := range cases {
		if got := shellConfirmationVerdict(text); got != want {
			t.Fatalf("verdict(%q) = %q, want %q", text, got, want)
		}
	}
}
