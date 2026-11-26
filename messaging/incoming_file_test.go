package messaging

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestIsTextFile(t *testing.T) {
	cases := []struct {
		name string
		data []byte
		want bool
	}{
		{"main.go", []byte("package main\n"), true},
		{"notes.txt", []byte("hello"), true},
		{"photo.png", []byte("not really a png"), false},
		{"script", []byte("#!/bin/sh\necho hi\n"), true},
		{"blob", []byte{0x00, 0x01, 0x02}, false},
		{"gbk.txt", []byte{0xd6, 0xd0}, true},
	}
	for _, tc := range cases {
		if got := isTextFile(tc.name, tc.data); got != tc.want {
			t.Errorf("isTextFile(%q) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestDecodeIncomingText(t *testing.T) {
	got, truncated := decodeIncomingText([]byte("\ufeffpackage main\n"))
	if got != "package main\n" || truncated {
		t.Fatalf("BOM decode = %q, truncated=%v", got, truncated)
	}

	long := strings.Repeat("a", incomingTextMaxChars+50)
	got, truncated = decodeIncomingText([]byte(long))
	if !truncated {
		t.Fatal("long text was not marked truncated")
	}
	if n := len([]rune(got)); n != incomingTextMaxChars {
		t.Fatalf("long text kept %d chars, want %d", n, incomingTextMaxChars)
	}
}

func TestAttachPendingFileEmbedsContentForHTTP(t *testing.T) {
	h := NewHandler(nil, nil)
	h.pendingFiles.Store("u", pendingIncomingFile{
		Name:     "main.go",
		Path:     "/tmp/main.go",
		Content:  "package main\n",
		Received: time.Now(),
	})

	got := h.attachPendingIncomingFile("u", "http", "帮我看看这段代码")
	if !strings.Contains(got, "package main") {
		t.Fatalf("content missing from prompt: %q", got)
	}
	if !strings.Contains(got, "帮我看看这段代码") {
		t.Fatalf("question missing from prompt: %q", got)
	}
	if _, ok := h.pendingFiles.Load("u"); !ok {
		t.Fatal("pending file should stay available for retries")
	}
	retry := h.attachPendingIncomingFile("u", "http", "那这段代码呢")
	if !strings.Contains(retry, "package main") {
		t.Fatalf("retry did not get the file body: %q", retry)
	}
}

func TestAttachPendingFileUsesPathForFileToolAgents(t *testing.T) {
	h := NewHandler(nil, nil)
	h.pendingFiles.Store("u", pendingIncomingFile{
		Name:     "main.go",
		Path:     "/tmp/main.go",
		Content:  "package main\n",
		Received: time.Now(),
	})

	got := h.attachPendingIncomingFile("u", "acp", "帮我看看这段代码")
	if !strings.Contains(got, "/tmp/main.go") {
		t.Fatalf("path missing from prompt: %q", got)
	}
	if strings.Contains(got, "package main") {
		t.Fatalf("ACP prompt should not embed the body: %q", got)
	}
}

func TestAttachPendingFileIgnoresUnrelatedChat(t *testing.T) {
	h := NewHandler(nil, nil)
	h.pendingFiles.Store("u", pendingIncomingFile{
		Name:     "main.go",
		Path:     "/tmp/main.go",
		Content:  "package main\n",
		Received: time.Now(),
	})

	if got := h.attachPendingIncomingFile("u", "http", "今天吃什么"); got != "今天吃什么" {
		t.Fatalf("unrelated chat was rewritten: %q", got)
	}
	if _, ok := h.pendingFiles.Load("u"); !ok {
		t.Fatal("pending file should survive unrelated chat")
	}
}

func TestPendingFileExpires(t *testing.T) {
	h := NewHandler(nil, nil)
	h.pendingFiles.Store("u", pendingIncomingFile{
		Name:     "main.go",
		Received: time.Now().Add(-2 * pendingIncomingTTL),
	})
	if _, ok := h.currentPendingIncomingFile("u"); ok {
		t.Fatal("expired pending file was returned")
	}
	if _, ok := h.pendingFiles.Load("u"); ok {
		t.Fatal("expired pending file was not removed")
	}
}

func TestFindRecentIncomingFile(t *testing.T) {
	h := NewHandler(nil, nil)
	dir := t.TempDir()
	h.SetSaveDir(dir)

	path := filepath.Join(dir, "152103_main.go")
	if err := os.WriteFile(path, []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sidecar := "---\nfrom_user_id: u\n---\n"
	if err := os.WriteFile(path+".sidecar.md", []byte(sidecar), 0o644); err != nil {
		t.Fatal(err)
	}

	p, ok := h.findRecentIncomingFile("u")
	if !ok {
		t.Fatal("recent upload not found")
	}
	if p.Name != "main.go" || p.Path != path || !strings.Contains(p.Content, "package main") {
		t.Fatalf("unexpected recovered file: %+v", p)
	}
	if _, ok := h.findRecentIncomingFile("someone-else"); ok {
		t.Fatal("recovered another user's file")
	}
}

func TestFindRecentIncomingFileSkipsImagesButKeepsZipFallback(t *testing.T) {
	h := NewHandler(nil, nil)
	dir := t.TempDir()
	h.SetSaveDir(dir)

	goPath := filepath.Join(dir, "152103_main.go")
	if err := os.WriteFile(goPath, []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(goPath+".sidecar.md", []byte("---\nfrom_user_id: u\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * time.Minute)
	if err := os.Chtimes(goPath, old, old); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(goPath+".sidecar.md", old, old); err != nil {
		t.Fatal(err)
	}

	imagePath := filepath.Join(dir, "203000_avatar.png")
	if err := os.WriteFile(imagePath, []byte("not really a png"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(imagePath+".sidecar.md", []byte("---\nfrom_user_id: u\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	p, ok := h.findRecentIncomingFile("u")
	if !ok || p.Name != "main.go" {
		t.Fatalf("recovered %+v, want main.go; image sidecar must not win", p)
	}

	zipPath := filepath.Join(dir, "203100_archive.zip")
	if err := os.WriteFile(zipPath, []byte("PK\x03\x04"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(zipPath+".sidecar.md", []byte("---\nfrom_user_id: u\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if p, ok := h.findRecentIncomingFileWithExt("u", ".zip"); !ok || p.Name != "archive.zip" {
		t.Fatalf("zip fallback recovered %+v, want archive.zip", p)
	}
}

func TestFormatIncomingSize(t *testing.T) {
	cases := map[int]string{
		512:             "512B",
		8*1024 + 512:    "8.5K",
		2 * 1024 * 1024: "2.0M",
	}
	for n, want := range cases {
		if got := formatIncomingSize(n); got != want {
			t.Errorf("formatIncomingSize(%d) = %q, want %q", n, got, want)
		}
	}
}
