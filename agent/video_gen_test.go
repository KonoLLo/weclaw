package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestVideoGenSubmitAndPoll(t *testing.T) {
	var polls int32
	var got struct {
		Model     string `json:"model"`
		Prompt    string `json:"prompt"`
		Size      string `json:"size"`
		Quality   string `json:"quality"`
		WithAudio bool   `json:"with_audio"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("authorization = %q", r.Header.Get("Authorization"))
		}
		switch r.URL.Path {
		case "/videos/generations":
			if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
				t.Errorf("decode submit: %v", err)
			}
			_, _ = w.Write([]byte(`{"id":"task-1","model":"cogvideox-flash","task_status":"PROCESSING"}`))
		case "/async-result/task-1":
			if atomic.AddInt32(&polls, 1) == 1 {
				_, _ = w.Write([]byte(`{"id":"task-1","task_status":"PROCESSING"}`))
				return
			}
			_, _ = w.Write([]byte(`{"id":"task-1","task_status":"SUCCESS","video_result":[{"url":"https://cdn.example/v.mp4"}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	g := NewVideoGen(srv.URL+"/videos/generations", "test-key", "", 2*time.Second)
	g.pollInterval = 10 * time.Millisecond
	g.SetSize("1080x1080")
	url, err := g.GenerateVideo(context.Background(), "一只橘猫追蝴蝶", "1440x720")
	if err != nil {
		t.Fatalf("GenerateVideo: %v", err)
	}
	if url != "https://cdn.example/v.mp4" {
		t.Fatalf("url = %q", url)
	}
	if got.Model != "cogvideox-flash" || got.Prompt != "一只橘猫追蝴蝶" || got.Size != "2048x1080" {
		t.Fatalf("submit body = %+v", got)
	}
	if got.Quality != "speed" || got.WithAudio {
		t.Fatalf("quality/audio = %q/%v", got.Quality, got.WithAudio)
	}
	if atomic.LoadInt32(&polls) != 2 {
		t.Fatalf("polls = %d, want 2", polls)
	}
}

func TestVideoGenReportsFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/videos/generations":
			_, _ = w.Write([]byte(`{"id":"task-2","task_status":"PROCESSING"}`))
		case "/async-result/task-2":
			_, _ = w.Write([]byte(`{"id":"task-2","task_status":"FAIL","error":{"code":"1301","message":"内容不合规"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	g := NewVideoGen(srv.URL+"/videos/generations", "", "", 2*time.Second)
	g.pollInterval = 10 * time.Millisecond
	if _, err := g.GenerateVideo(context.Background(), "测试", ""); err == nil || !strings.Contains(err.Error(), "内容不合规") {
		t.Fatalf("err = %v", err)
	}
}

func TestVideoGenReportsAPIErrorImmediately(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/videos/generations":
			_, _ = w.Write([]byte(`{"id":"task-3","task_status":"PROCESSING"}`))
		case "/async-result/task-3":
			_, _ = w.Write([]byte(`{"error":{"code":"1301","message":"系统检测到输入或生成内容可能包含不安全内容"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	g := NewVideoGen(srv.URL+"/videos/generations", "", "", 10*time.Second)
	g.pollInterval = 10 * time.Millisecond
	start := time.Now()
	_, err := g.GenerateVideo(context.Background(), "测试", "")
	if err == nil || !strings.Contains(err.Error(), "1301") {
		t.Fatalf("err = %v", err)
	}
	if time.Since(start) > time.Second {
		t.Fatalf("API error waited %s, want immediate", time.Since(start))
	}
}

func TestNormalizeVideoSize(t *testing.T) {
	cases := map[string]string{
		"1920x1080": "1920x1080",
		"1440x720":  "2048x1080",
		"1080x1920": "1080x1920",
		"720×480":   "720x480",
		"3840x2160": "3840x2160",
		"":          "",
		"abc":       "",
	}
	for in, want := range cases {
		if got := NormalizeVideoSize(in); got != want {
			t.Errorf("NormalizeVideoSize(%q) = %q, want %q", in, got, want)
		}
	}
}
