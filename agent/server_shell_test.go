package agent

import (
	"context"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestServerShellRunsCommand(t *testing.T) {
	sh := NewServerShell(0, 0)
	command := `printf hello`
	if runtime.GOOS == "windows" {
		command = `echo hello`
	}
	out, err := sh.Run(context.Background(), command)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if !strings.Contains(out, "hello") {
		t.Fatalf("output = %q, want to contain hello", out)
	}
}

func TestServerShellRejectsEmptyCommand(t *testing.T) {
	sh := NewServerShell(0, 0)
	if _, err := sh.Run(context.Background(), "   "); err == nil {
		t.Fatal("expected error for empty command")
	}
}

func TestServerShellBlocksDangerousCommands(t *testing.T) {
	sh := NewServerShell(0, 0)
	for _, command := range []string{
		"rm -rf /",
		"rm -rf /*",
		"mkfs.ext4 /dev/sda1",
		"dd if=/dev/zero of=/dev/sda",
		"shutdown -h now",
		"reboot",
		"userdel someone",
	} {
		if _, err := sh.Run(context.Background(), command); err == nil || !strings.Contains(err.Error(), "安全策略") {
			t.Fatalf("command %q was not blocked: %v", command, err)
		}
	}
}

func TestServerShellAllowsServiceCommands(t *testing.T) {
	for _, command := range []string{
		"docker ps -a",
		"systemctl status weclaw",
		"journalctl -u weclaw -n 20",
		"df -h",
		"rm -rf ./tmp-cache",
	} {
		for _, re := range blockedShellPatterns {
			if re.MatchString(command) {
				t.Fatalf("command %q was blocked by %v", command, re)
			}
		}
	}
}

func TestServerShellTimeout(t *testing.T) {
	sh := &ServerShell{Timeout: 300 * time.Millisecond, MaxOutputBytes: 1024}
	command := "sleep 3"
	if runtime.GOOS == "windows" {
		command = "ping -n 4 127.0.0.1"
	}
	if _, err := sh.Run(context.Background(), command); err == nil || !strings.Contains(err.Error(), "超时") {
		t.Fatalf("expected timeout error, got %v", err)
	}
}

func TestTruncateShellOutputKeepsHeadAndTail(t *testing.T) {
	out := strings.Repeat("a", 100) + strings.Repeat("z", 100)
	got := truncateShellOutput(out, 40)
	if !strings.HasPrefix(got, strings.Repeat("a", 20)) {
		t.Fatalf("head missing: %q", got)
	}
	if !strings.HasSuffix(got, strings.Repeat("z", 20)) {
		t.Fatalf("tail missing: %q", got)
	}
	if !strings.Contains(got, "截断") {
		t.Fatalf("truncation marker missing: %q", got)
	}
}
