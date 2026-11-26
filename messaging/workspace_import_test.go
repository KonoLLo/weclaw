package messaging

import (
	"archive/zip"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTestZip(t *testing.T, entries map[string]string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "demo.zip")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	for name, body := range entries {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestNormalizeGitHubRepo(t *testing.T) {
	valid := map[string]string{
		"https://github.com/owner/repo":      "https://github.com/owner/repo",
		"https://github.com/owner/repo.git":  "https://github.com/owner/repo",
		"github.com/owner/repo/":             "https://github.com/owner/repo",
		"https://github.com/my-org/my_repo":  "https://github.com/my-org/my_repo",
		"https://github.com/owner/repo.name": "https://github.com/owner/repo.name",
	}
	for input, want := range valid {
		got, _, err := normalizeGitHubRepo(input)
		if err != nil || got != want {
			t.Errorf("normalizeGitHubRepo(%q) = %q, %v; want %q", input, got, err, want)
		}
	}
	invalid := []string{
		"http://github.com/owner/repo",
		"https://gitlab.com/owner/repo",
		"https://github.com/owner",
		"https://github.com/owner/repo/tree/main",
		"https://user@github.com/owner/repo",
		"https://github.com/owner/repo?x=1",
		"git@github.com:owner/repo.git",
	}
	for _, input := range invalid {
		if _, _, err := normalizeGitHubRepo(input); err == nil {
			t.Errorf("normalizeGitHubRepo(%q) should fail", input)
		}
	}
}

func TestImportWorkspaceFromZip(t *testing.T) {
	m, _ := newTestWorkspaceManager(t, false)
	ctx := context.Background()
	zipPath := writeTestZip(t, map[string]string{
		"proj/README.md":   "# demo\n",
		"proj/src/main.go": "package main\n",
	})
	msg := m.importWorkspace(ctx, "user", "", zipPath, "demo.zip")
	if !strings.Contains(msg, "已导入工作区 demo") {
		t.Fatalf("importWorkspace = %q", msg)
	}
	root, name, err := m.currentRoot(ctx, "user", false)
	if err != nil {
		t.Fatal(err)
	}
	if name != "demo" {
		t.Fatalf("current workspace = %q", name)
	}
	if got := m.readFile(ctx, "user", "README.md"); !strings.Contains(got, "# demo") {
		t.Fatalf("flattened README = %q", got)
	}
	if got := m.readFile(ctx, "user", "src/main.go"); !strings.Contains(got, "package main") {
		t.Fatalf("source = %q", got)
	}
	if !isWithinRoot(root, filepath.Join(root, "README.md")) {
		t.Fatal("workspace root mismatch")
	}
}

func TestExtractZipRejectsTraversalAndSymlinks(t *testing.T) {
	m, _ := newTestWorkspaceManager(t, false)
	ctx := context.Background()

	badPath := writeTestZip(t, map[string]string{"../evil.txt": "x"})
	if _, _, err := m.extractZip(ctx, badPath, t.TempDir()); err == nil || !strings.Contains(err.Error(), "不安全路径") {
		t.Fatalf("traversal zip error = %v", err)
	}

	linkZip := filepath.Join(t.TempDir(), "link.zip")
	f, err := os.Create(linkZip)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	h := &zip.FileHeader{Name: "link"}
	h.SetMode(os.ModeSymlink | 0o777)
	w, err := zw.CreateHeader(h)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.Write([]byte("/etc/passwd"))
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.extractZip(ctx, linkZip, t.TempDir()); err == nil || !strings.Contains(err.Error(), "符号链接") {
		t.Fatalf("symlink zip error = %v", err)
	}
}
