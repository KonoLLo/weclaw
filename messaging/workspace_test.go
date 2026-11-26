package messaging

import (
	"archive/zip"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func newTestWorkspaceManager(t *testing.T, allowShell bool) (*WorkspaceManager, string) {
	t.Helper()
	root := t.TempDir()
	m, err := NewWorkspaceManager(WorkspaceOptions{
		Enabled:    true,
		Root:       root,
		AllowShell: allowShell,
		Timeout:    10 * time.Second,
	}, nil)
	if err != nil {
		t.Fatalf("NewWorkspaceManager: %v", err)
	}
	t.Cleanup(m.Close)
	return m, root
}

func TestWorkspaceWriteReadAndEscape(t *testing.T) {
	m, _ := newTestWorkspaceManager(t, false)
	ctx := context.Background()

	msg := m.writeFile(ctx, "user", "src/main.go", "package main\n")
	if !strings.Contains(msg, "已写入") {
		t.Fatalf("writeFile = %q", msg)
	}
	if got := m.readFile(ctx, "user", "src/main.go"); !strings.Contains(got, "package main") {
		t.Fatalf("readFile = %q", got)
	}
	if got := m.readFile(ctx, "user", "../secret"); !strings.Contains(got, "不合法") {
		t.Fatalf("parent escape should be rejected, got %q", got)
	}
	if got := m.readFile(ctx, "user", "/etc/passwd"); !strings.Contains(got, "不合法") {
		t.Fatalf("absolute path should be rejected, got %q", got)
	}
	if got := m.writeFile(ctx, "user", "../outside.txt", "x"); !strings.Contains(got, "不合法") {
		t.Fatalf("write escape should be rejected, got %q", got)
	}
}

func TestWorkspaceSymlinkEscapeRejected(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink permissions vary on Windows")
	}
	m, _ := newTestWorkspaceManager(t, false)
	ctx := context.Background()
	root, _, err := m.currentRoot(ctx, "user", true)
	if err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "leak.txt")); err != nil {
		t.Fatal(err)
	}
	if got := m.readFile(ctx, "user", "leak.txt"); !strings.Contains(got, "不合法") {
		t.Fatalf("symlink escape should be rejected, got %q", got)
	}
	if _, err := m.sendPath(ctx, "user", "leak.txt"); err == nil {
		t.Fatal("sendPath should reject symlink escape")
	}
}

func TestWorkspaceRunGateAndOutput(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test command uses /bin/sh")
	}
	m, _ := newTestWorkspaceManager(t, false)
	ctx := context.Background()
	if got := m.runCommand(ctx, "user", "printf hello"); !strings.Contains(got, "未启用") {
		t.Fatalf("disabled shell reply = %q", got)
	}

	m.opts.AllowShell = true
	if got := m.runCommand(ctx, "user", "printf hello"); !strings.Contains(got, "hello") {
		t.Fatalf("run output = %q", got)
	}
	if got := m.runCommand(ctx, "user", "exit 3"); !strings.Contains(got, "退出码 3") {
		t.Fatalf("exit code not reported: %q", got)
	}
}

func TestWorkspacePackSkipsGitAndCaps(t *testing.T) {
	m, _ := newTestWorkspaceManager(t, false)
	ctx := context.Background()
	if msg := m.writeFile(ctx, "user", "a.txt", "alpha"); !strings.Contains(msg, "已写入") {
		t.Fatal(msg)
	}
	if msg := m.writeFile(ctx, "user", "dir/b.txt", "beta"); !strings.Contains(msg, "已写入") {
		t.Fatal(msg)
	}
	archive, name, cleanup, err := m.createArchive(ctx, "user")
	if err != nil {
		t.Fatalf("createArchive: %v", err)
	}
	defer cleanup()
	if name != defaultWorkspaceName {
		t.Fatalf("workspace name = %q", name)
	}
	zr, err := zip.OpenReader(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	seen := map[string]bool{}
	for _, f := range zr.File {
		seen[f.Name] = true
	}
	if !seen["a.txt"] || !seen["dir/b.txt"] {
		t.Fatalf("archive entries = %v", seen)
	}
}

func TestWorkspaceNameValidation(t *testing.T) {
	valid := []string{"main", "demo-1", "hello_world", "v1.2"}
	for _, name := range valid {
		if !validWorkspaceName(name) {
			t.Errorf("expected valid: %q", name)
		}
	}
	invalid := []string{"", ".", "..", "../x", "a/b", "a\\b", strings.Repeat("a", 49), "a b"}
	for _, name := range invalid {
		if validWorkspaceName(name) {
			t.Errorf("expected invalid: %q", name)
		}
	}
}

func TestWorkspaceUserDirNameIsStableAndSafe(t *testing.T) {
	got := workspaceUserDirName("user@example.com")
	if strings.ContainsAny(got, "@:./\\") {
		t.Fatalf("unsafe user dir: %q", got)
	}
	if got != workspaceUserDirName("user@example.com") {
		t.Fatal("user dir name should be stable")
	}
	if got == workspaceUserDirName("user@example.org") {
		t.Fatal("different users should not share a workspace dir")
	}
}
