package agent

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// removeDirCommand renders a destructive-but-legitimate cleanup for the host
// platform, so the staging test can later run the same string for real.
func removeDirCommand(dir string) string {
	if runtime.GOOS == "windows" {
		return `rmdir /s /q "` + dir + `"`
	}
	return "rm -rf " + dir
}

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

func TestServerShellStagesCleanupCommandForConfirmation(t *testing.T) {
	sh := NewServerShell(0, 0)
	dir := t.TempDir()
	target := filepath.Join(dir, "cache.txt")
	if err := os.WriteFile(target, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	defer ClearShellPending("u-stage")

	out, err := sh.RunFor(context.Background(), "u-stage", removeDirCommand(dir))
	if err != nil {
		t.Fatalf("RunFor returned error: %v", err)
	}
	if !strings.Contains(out, "确认") {
		t.Fatalf("staging hint missing: %q", out)
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("file must survive until the owner confirms: %v", err)
	}

	if peeked, _, ok := PeekShellPending("u-stage"); !ok || peeked != removeDirCommand(dir) {
		t.Fatalf("peek = %q ok=%v, want the staged command", peeked, ok)
	}
	if peeked, _, ok := PeekShellPending("u-stage"); !ok || peeked != removeDirCommand(dir) {
		t.Fatalf("peek must not consume the command")
	}

	command, ok := TakeShellPending("u-stage")
	if !ok || command != removeDirCommand(dir) {
		t.Fatalf("pending = %q ok=%v, want the staged command", command, ok)
	}
	if _, ok := TakeShellPending("u-stage"); ok {
		t.Fatal("pending must be consumed once")
	}
}

func TestServerShellRunsConfirmedCommand(t *testing.T) {
	sh := NewServerShell(0, 0)
	dir := t.TempDir()
	target := filepath.Join(dir, "cache.txt")
	if err := os.WriteFile(target, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := sh.RunConfirmed(context.Background(), removeDirCommand(dir)); err != nil {
		t.Fatalf("RunConfirmed returned error: %v", err)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("file should be gone after confirmation, stat err=%v", err)
	}
}

func TestServerShellStagedCommandExpires(t *testing.T) {
	defer ClearShellPending("u-expire")
	stageShellPending("u-expire", "rm -rf ./old")
	shellPendingStore.Lock()
	item := shellPendingStore.items["u-expire"]
	item.CreatedAt = time.Now().Add(-pendingShellTTL - time.Minute)
	shellPendingStore.items["u-expire"] = item
	shellPendingStore.Unlock()

	if _, ok := TakeShellPending("u-expire"); ok {
		t.Fatal("expired pending command must not be takeable")
	}
}

func TestServerShellRefusesProtectedTargets(t *testing.T) {
	sh := NewServerShell(0, 0)
	for _, command := range []string{
		"rm -rf /root/.weclaw",
		"rm -rf ~/.weclaw/accounts",
		"rm -rf /opt/qzsz",
		"systemctl stop qzsz-server",
		"systemctl restart mariadb",
		"systemctl disable sing-box",
		"pkill -f weclaw",
		"truncate -s 0 /var/lib/mysql/ibdata1",
	} {
		if _, err := sh.RunFor(context.Background(), "u-protect", command); err == nil || !strings.Contains(err.Error(), "受保护") {
			t.Fatalf("command %q was not refused: %v", command, err)
		}
	}
	if _, ok := TakeShellPending("u-protect"); ok {
		t.Fatal("protected commands must never be staged")
	}
}

func TestServerShellAllowsStatusOfProtectedService(t *testing.T) {
	sh := NewServerShell(0, 0)
	defer ClearShellPending("u-status")
	_, err := sh.RunFor(context.Background(), "u-status", "systemctl status qzsz-server")
	if err != nil && strings.Contains(err.Error(), "受保护") {
		t.Fatalf("read-only status was refused: %v", err)
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
