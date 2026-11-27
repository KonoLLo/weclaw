package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestHTTPAgentChatWithAttachmentsUsesNativeMediaContent(t *testing.T) {
	for _, model := range []string{"MiniMax-M3.1-Flash", "MiniMax-M3"} {
		t.Run(model, func(t *testing.T) {
			testHTTPAgentChatWithAttachmentsUsesNativeMediaContent(t, model)
		})
	}
}

func testHTTPAgentChatWithAttachmentsUsesNativeMediaContent(t *testing.T, model string) {
	var request struct {
		Messages []struct {
			Role    string `json:"role"`
			Content any    `json:"content"`
		} `json:"messages"`
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"done"}}]}`))
	}))
	defer server.Close()

	dir := t.TempDir()
	imagePath := filepath.Join(dir, "sample.png")
	videoPath := filepath.Join(dir, "sample.mp4")
	if err := os.WriteFile(imagePath, []byte("image"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(videoPath, []byte("video"), 0o600); err != nil {
		t.Fatal(err)
	}

	ag := NewHTTPAgent(HTTPAgentConfig{Endpoint: server.URL, Model: model})
	reply, err := ag.ChatWithAttachments(context.Background(), "conversation", "Describe the media.", []Attachment{
		{Path: imagePath, MIMEType: "image/png", Name: "sample.png"},
		{Path: videoPath, MIMEType: "video/mp4", Name: "sample.mp4"},
	})
	if err != nil {
		t.Fatalf("ChatWithAttachments returned error: %v", err)
	}
	if reply != "done" {
		t.Fatalf("reply = %q, want done", reply)
	}

	content, ok := request.Messages[len(request.Messages)-1].Content.([]any)
	if !ok {
		t.Fatalf("user content type = %T, want array", request.Messages[len(request.Messages)-1].Content)
	}
	if len(content) != 3 {
		t.Fatalf("content part count = %d, want 3", len(content))
	}
	assertContentPartType(t, content[0], "text", "text", "Describe the media.")
	assertContentPartType(t, content[1], "image_url", "image_url", "data:image/png;base64,aW1hZ2U=")
	assertContentPartType(t, content[2], "video_url", "video_url", "data:video/mp4;base64,dmlkZW8=")
}

func TestSupportsNativeMedia(t *testing.T) {
	image := []Attachment{{Path: "/tmp/sample.png", MIMEType: "image/png"}}
	video := []Attachment{{Path: "/tmp/sample.mp4", MIMEType: "video/mp4"}}
	cases := []struct {
		name        string
		model       string
		attachments []Attachment
		want        bool
	}{
		{"minimax image+video", "MiniMax-M3.1-Flash", append(append([]Attachment{}, image...), video...), true},
		{"gemini image", "gemini-3.1-flash-lite", image, true},
		{"gemini image uppercase", "Gemini-4-Hydrogen", image, true},
		{"gemini video", "gemini-3.1-flash-lite", video, false},
		{"text-only", "text-only-model", image, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := supportsNativeMedia(tc.model, tc.attachments); got != tc.want {
				t.Fatalf("supportsNativeMedia(%q) = %v, want %v", tc.model, got, tc.want)
			}
		})
	}
}

func TestHTTPAgentChatWithAttachmentsUsesNativeImageForGemini(t *testing.T) {
	var content any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Messages []struct {
				Content any `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode request: %v", err)
		}
		content = request.Messages[len(request.Messages)-1].Content
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"done"}}]}`))
	}))
	defer server.Close()

	imagePath := filepath.Join(t.TempDir(), "sample.png")
	if err := os.WriteFile(imagePath, []byte("image"), 0o600); err != nil {
		t.Fatal(err)
	}
	ag := NewHTTPAgent(HTTPAgentConfig{Endpoint: server.URL, Model: "gemini-3.1-flash-lite"})
	if _, err := ag.ChatWithAttachments(context.Background(), "conversation", "Describe the image.", []Attachment{{
		Path: imagePath, MIMEType: "image/png", Name: "sample.png",
	}}); err != nil {
		t.Fatalf("ChatWithAttachments returned error: %v", err)
	}

	parts, ok := content.([]any)
	if !ok {
		t.Fatalf("user content type = %T, want array", content)
	}
	if len(parts) != 2 {
		t.Fatalf("content part count = %d, want 2", len(parts))
	}
	assertContentPartType(t, parts[0], "text", "text", "Describe the image.")
	assertContentPartType(t, parts[1], "image_url", "image_url", "data:image/png;base64,aW1hZ2U=")
}

func TestHTTPAgentChatWithAttachmentsFallsBackForTextOnlyModel(t *testing.T) {
	var content any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Messages []struct {
				Content any `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode request: %v", err)
		}
		content = request.Messages[len(request.Messages)-1].Content
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"done"}}]}`))
	}))
	defer server.Close()

	ag := NewHTTPAgent(HTTPAgentConfig{Endpoint: server.URL, Model: "text-only-model"})
	_, err := ag.ChatWithAttachments(context.Background(), "conversation", "Describe this.", []Attachment{{
		Path:     "/tmp/sample.png",
		MIMEType: "image/png",
		Name:     "sample.png",
	}})
	if err != nil {
		t.Fatalf("ChatWithAttachments returned error: %v", err)
	}
	text, ok := content.(string)
	if !ok {
		t.Fatalf("user content type = %T, want string", content)
	}
	if !strings.Contains(text, "/tmp/sample.png") {
		t.Fatalf("fallback content does not contain attachment path: %q", text)
	}
}

func TestHTTPAgentRetriesServiceUnavailableOnce(t *testing.T) {
	oldDelay := httpRetryDelay
	httpRetryDelay = 10 * time.Millisecond
	defer func() { httpRetryDelay = oldDelay }()

	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":{"code":503,"status":"UNAVAILABLE"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer server.Close()

	ag := NewHTTPAgent(HTTPAgentConfig{Endpoint: server.URL, Model: "gemini-3.1-flash-lite"})
	reply, err := ag.Chat(context.Background(), "u-retry", "hi")
	if err != nil {
		t.Fatalf("Chat returned error: %v", err)
	}
	if reply != "ok" {
		t.Fatalf("reply = %q, want ok", reply)
	}
	if calls != 2 {
		t.Fatalf("calls = %d, want 2 (one retry)", calls)
	}
}

func TestHTTPAgentFriendlyErrorAfterRetry(t *testing.T) {
	oldDelay := httpRetryDelay
	httpRetryDelay = 10 * time.Millisecond
	defer func() { httpRetryDelay = oldDelay }()

	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":{"code":503,"message":"This model is currently experiencing high demand"}}`))
	}))
	defer server.Close()

	ag := NewHTTPAgent(HTTPAgentConfig{Endpoint: server.URL, Model: "gemini-3.1-flash-lite"})
	_, err := ag.Chat(context.Background(), "u-503", "hi")
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), `"error"`) || strings.Contains(err.Error(), "UNAVAILABLE") {
		t.Fatalf("raw provider body leaked to user: %v", err)
	}
	if !strings.Contains(err.Error(), "太忙") {
		t.Fatalf("error should be friendly Chinese: %v", err)
	}
	if calls != 2 {
		t.Fatalf("calls = %d, want 2", calls)
	}
}

func TestHTTPAgentDoesNotRetryAuthErrors(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"invalid key"}}`))
	}))
	defer server.Close()

	ag := NewHTTPAgent(HTTPAgentConfig{Endpoint: server.URL, Model: "m"})
	_, err := ag.Chat(context.Background(), "u-401", "hi")
	if err == nil || !strings.Contains(err.Error(), "API Key") {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want 1 (no retry on 401)", calls)
	}
}

func TestHTTPAgentRunsServerShellToolLoop(t *testing.T) {
	type wireMessage struct {
		Role       string     `json:"role"`
		Content    any        `json:"content"`
		ToolCalls  []ToolCall `json:"tool_calls"`
		ToolCallID string     `json:"tool_call_id"`
	}
	type wireRequest struct {
		Tools    []map[string]any `json:"tools"`
		Messages []wireMessage    `json:"messages"`
	}
	var requests []wireRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req wireRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
		}
		requests = append(requests, req)
		w.Header().Set("Content-Type", "application/json")
		if len(requests) == 1 {
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"server_shell","arguments":"{\"command\":\"echo tool-ok\"}"},"extra_content":{"google":{"thought_signature":"sig-1"}}}]}}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"final"}}]}`))
	}))
	defer server.Close()

	sh := NewServerShell(0, 0)
	ag := NewHTTPAgent(HTTPAgentConfig{Endpoint: server.URL, Model: "m", Shell: sh})
	if _, err := ag.Chat(context.Background(), "u-tool", "run something"); err != nil {
		t.Fatalf("Chat returned error: %v", err)
	}

	if len(requests) != 2 {
		t.Fatalf("requests = %d, want 2", len(requests))
	}
	if len(requests[0].Tools) != 1 {
		t.Fatalf("first request tools = %d, want 1", len(requests[0].Tools))
	}
	fn, _ := requests[0].Tools[0]["function"].(map[string]any)
	if fn["name"] != serverShellToolName {
		t.Fatalf("tool name = %v, want %s", fn["name"], serverShellToolName)
	}
	last := requests[1].Messages[len(requests[1].Messages)-1]
	if last.Role != "tool" || last.ToolCallID != "call_1" {
		t.Fatalf("last message role=%q tool_call_id=%q, want tool/call_1", last.Role, last.ToolCallID)
	}
	if out, _ := last.Content.(string); !strings.Contains(out, "tool-ok") {
		t.Fatalf("tool output = %v, want to contain tool-ok", last.Content)
	}
	assistant := requests[1].Messages[len(requests[1].Messages)-2]
	if len(assistant.ToolCalls) != 1 {
		t.Fatalf("assistant tool_calls = %d, want 1", len(assistant.ToolCalls))
	}
	if sig := string(assistant.ToolCalls[0].ExtraContent); !strings.Contains(sig, "sig-1") {
		t.Fatalf("extra_content = %q, want the provider signature echoed back", sig)
	}
}

func TestHTTPAgentToolLoopStagesConfirmationCommand(t *testing.T) {
	type wireMessage struct {
		Role    string `json:"role"`
		Content any    `json:"content"`
	}
	type wireRequest struct {
		Messages []wireMessage `json:"messages"`
	}
	var requests []wireRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req wireRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
		}
		requests = append(requests, req)
		w.Header().Set("Content-Type", "application/json")
		if len(requests) == 1 {
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"","tool_calls":[{"id":"call_c","type":"function","function":{"name":"server_shell","arguments":"{\"command\":\"rm -rf ./build-cache\"}"}}]}}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"请确认"}}]}`))
	}))
	defer server.Close()

	defer ClearShellPending("u-confirm")
	ag := NewHTTPAgent(HTTPAgentConfig{Endpoint: server.URL, Model: "m", Shell: NewServerShell(0, 0)})
	if _, err := ag.Chat(context.Background(), "u-confirm", "清理一下缓存"); err != nil {
		t.Fatalf("Chat returned error: %v", err)
	}
	if len(requests) != 2 {
		t.Fatalf("requests = %d, want 2", len(requests))
	}
	last := requests[1].Messages[len(requests[1].Messages)-1]
	if last.Role != "tool" {
		t.Fatalf("last message role = %q, want tool", last.Role)
	}
	if out, _ := last.Content.(string); !strings.Contains(out, "用户本人确认") {
		t.Fatalf("tool output = %v, want a confirmation request", last.Content)
	}
	command, ok := TakeShellPending("u-confirm")
	if !ok || command != "rm -rf ./build-cache" {
		t.Fatalf("pending = %q ok=%v, want the destructive command staged", command, ok)
	}
}

func TestFriendlyHTTPErrorUsesProviderMessage(t *testing.T) {
	body := `[{ "error": { "code": 400, "message": "Function call is missing a thought_signature in functionCall parts." } }]`
	err := friendlyHTTPError(400, body)
	if err == nil || !strings.Contains(err.Error(), "thought_signature") {
		t.Fatalf("error = %v, want the provider message", err)
	}
	if strings.Contains(err.Error(), `"code"`) {
		t.Fatalf("error = %v, want raw JSON stripped", err)
	}
}

func TestHTTPAgentFallsBackWhenProviderRejectsTools(t *testing.T) {
	var withTools, withoutTools int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Tools []map[string]any `json:"tools"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		if len(req.Tools) > 0 {
			withTools++
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"tools is not supported"}}`))
			return
		}
		withoutTools++
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"plain"}}]}`))
	}))
	defer server.Close()

	ag := NewHTTPAgent(HTTPAgentConfig{Endpoint: server.URL, Model: "m", Shell: NewServerShell(0, 0)})
	reply, err := ag.Chat(context.Background(), "u-notools", "hi")
	if err != nil {
		t.Fatalf("Chat returned error: %v", err)
	}
	if reply != "plain" {
		t.Fatalf("reply = %q, want plain", reply)
	}
	if withTools != 1 || withoutTools != 1 {
		t.Fatalf("calls with/without tools = %d/%d, want 1/1", withTools, withoutTools)
	}
}

func assertContentPartType(t *testing.T, raw any, wantType, wantPayload, wantValue string) {
	t.Helper()
	part, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("content part type = %T, want object", raw)
	}
	if part["type"] != wantType {
		t.Fatalf("content part type = %v, want %q", part["type"], wantType)
	}
	payload, ok := part[wantPayload]
	if !ok {
		t.Fatalf("content part missing %q: %#v", wantPayload, part)
	}
	if wantPayload == "text" {
		if payload != wantValue {
			t.Fatalf("text payload = %v, want %q", payload, wantValue)
		}
		return
	}
	media, ok := payload.(map[string]any)
	if !ok || media["url"] != wantValue {
		t.Fatalf("media payload = %#v, want URL %q", payload, wantValue)
	}
}
