package agent

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// ChatMessage represents a single message in a conversation.
type ChatMessage struct {
	Role       string     `json:"role"`
	Content    any        `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

// ToolCall is the OpenAI-compatible function call shape used by server_shell.
type ToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type chatContentPart struct {
	Type     string        `json:"type"`
	Text     string        `json:"text,omitempty"`
	ImageURL *chatMediaURL `json:"image_url,omitempty"`
	VideoURL *chatMediaURL `json:"video_url,omitempty"`
}

type chatMediaURL struct {
	URL string `json:"url"`
}

// HTTPAgent is an OpenAI-compatible chat completions API client.
type HTTPAgent struct {
	endpoint     string
	apiKey       string
	headers      map[string]string
	model        string
	systemPrompt string
	httpClient   *http.Client
	mu           sync.Mutex
	history      map[string][]ChatMessage // conversationID -> messages
	maxHistory   int
	models       []string
	stateKey     string
	shell        *ServerShell
}

// DefaultHTTPTimeout is the request timeout for HTTP agents when the config
// does not override it. 60s was too tight for agent turns that include
// context compaction or a slow model call (observed via WeChat timeouts).
// File analysis can take longer when the provider is congested, so the ceiling
// is 300s; normal replies still return in well under a minute.
const DefaultHTTPTimeout = 300 * time.Second

// HTTPAgentConfig holds configuration for the HTTP agent.
type HTTPAgentConfig struct {
	Endpoint     string
	APIKey       string
	Headers      map[string]string
	Model        string
	SystemPrompt string
	MaxHistory   int
	Timeout      time.Duration
	Models       []string
	Name         string
	Shell        *ServerShell
}

// NewHTTPAgent creates a new OpenAI-compatible HTTP agent.
func NewHTTPAgent(cfg HTTPAgentConfig) *HTTPAgent {
	if cfg.MaxHistory == 0 {
		cfg.MaxHistory = 20
	}
	if cfg.Model == "" {
		cfg.Model = "gpt-4o-mini"
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = DefaultHTTPTimeout
	}
	ag := &HTTPAgent{
		endpoint:     cfg.Endpoint,
		apiKey:       cfg.APIKey,
		headers:      cfg.Headers,
		model:        cfg.Model,
		systemPrompt: cfg.SystemPrompt,
		httpClient:   &http.Client{Timeout: cfg.Timeout},
		history:      make(map[string][]ChatMessage),
		maxHistory:   cfg.MaxHistory,
		models:       cfg.Models,
		stateKey:     cfg.Name,
		shell:        cfg.Shell,
	}
	if ag.stateKey == "" {
		ag.stateKey = "http"
	}
	if sel := loadModelSelection(ag.stateKey); sel != "" {
		for _, m := range ag.models {
			if m == sel {
				ag.model = m
				break
			}
		}
	}
	return ag
}

// Info returns metadata about this agent.
func (a *HTTPAgent) Info() AgentInfo {
	return AgentInfo{
		Name:    "http",
		Type:    "http",
		Model:   a.currentModel(),
		Command: a.endpoint,
	}
}

// SetCwd is a no-op for HTTP agents (they have no working directory).
func (a *HTTPAgent) SetCwd(_ string) {}

// ResetSession clears the conversation history for the given conversationID.
// HTTP agents have no server-side session ID, so an empty string is returned.
func (a *HTTPAgent) ResetSession(_ context.Context, conversationID string) (string, error) {
	a.mu.Lock()
	delete(a.history, conversationID)
	a.mu.Unlock()
	return "", nil
}

// Chat sends a message to the OpenAI-compatible API and returns the response.
func (a *HTTPAgent) Chat(ctx context.Context, conversationID string, message string) (string, error) {
	return a.chat(ctx, conversationID, message)
}

// ChatWithAttachments sends supported media as native content parts.
func (a *HTTPAgent) ChatWithAttachments(ctx context.Context, conversationID string, message string, attachments []Attachment) (string, error) {
	if len(attachments) == 0 {
		return a.Chat(ctx, conversationID, message)
	}
	if !supportsNativeMedia(a.currentModel(), attachments) {
		return a.Chat(ctx, conversationID, MessageWithAttachmentFallback(message, attachments))
	}

	content, err := buildNativeMediaContent(message, attachments)
	if err != nil {
		return "", err
	}
	return a.chat(ctx, conversationID, content)
}

// supportsNativeMedia is deliberately a whitelist: sending an image_url to a
// text-only model turns a clear fallback into a provider-specific 400. Gemini
// is image-only here because its OpenAI-compatible endpoint accepts image data
// URLs but not the video_url shape used by MiniMax.
func supportsNativeMedia(model string, attachments []Attachment) bool {
	name := strings.ToLower(strings.TrimSpace(model))
	switch name {
	case "minimax-m3.1-flash", "minimax-m3":
		return true
	}
	if !strings.HasPrefix(name, "gemini-") {
		return false
	}
	for _, attachment := range attachments {
		mediaType := strings.ToLower(strings.TrimSpace(strings.Split(attachment.MIMEType, ";")[0]))
		if mediaType == "" {
			mediaType = strings.ToLower(mime.TypeByExtension(filepath.Ext(attachment.Path)))
		}
		if !strings.HasPrefix(mediaType, "image/") {
			return false
		}
	}
	return true
}

// ChatClean answers a one-off prompt without the persona system prompt or
// conversation history.
func (a *HTTPAgent) ChatClean(ctx context.Context, message string) (string, error) {
	return a.complete(ctx, []ChatMessage{{Role: "user", Content: message}})
}

// A fast 503/429 (immediate rejection, common on free tiers) is worth one
// retry; a failure that already burned tens of seconds in the provider queue
// will not get better by waiting again.
const httpRetryMaxElapsed = 20 * time.Second

var httpRetryDelay = 2 * time.Second

// completion is one parsed chat.completions response. status/body stay set
// when the provider rejected the request so callers can log and map errors.
type completion struct {
	content   string
	toolCalls []ToolCall
	status    int
	body      string
}

const maxToolRounds = 5

func (a *HTTPAgent) complete(ctx context.Context, messages []ChatMessage) (string, error) {
	res, err := a.completeOnce(ctx, messages, false)
	return res.content, err
}

func (a *HTTPAgent) completeOnce(ctx context.Context, messages []ChatMessage, withTools bool) (completion, error) {
	reqBody := map[string]interface{}{
		"model":    a.currentModel(),
		"messages": messages,
	}
	if withTools {
		reqBody["tools"] = serverShellTools
	}

	data, err := json.Marshal(reqBody)
	if err != nil {
		return completion{}, fmt.Errorf("marshal request: %w", err)
	}

	start := time.Now()
	res, err := a.doComplete(ctx, data)
	if err == nil {
		return res, nil
	}
	if res.status == 0 {
		return completion{}, err
	}
	log.Printf("[http] %s HTTP %d: %s", a.currentModel(), res.status, truncateForLog(res.body, 400))
	if retryableHTTPStatus(res.status) && time.Since(start) < httpRetryMaxElapsed && ctx.Err() == nil {
		log.Printf("[http] retrying %s once after HTTP %d", a.currentModel(), res.status)
		select {
		case <-ctx.Done():
			return completion{}, ctx.Err()
		case <-time.After(httpRetryDelay):
		}
		res, err = a.doComplete(ctx, data)
		if err == nil {
			return res, nil
		}
		if res.status == 0 {
			return completion{}, err
		}
		log.Printf("[http] %s HTTP %d after retry: %s", a.currentModel(), res.status, truncateForLog(res.body, 400))
	}
	return completion{status: res.status, body: res.body}, friendlyHTTPError(res.status, res.body)
}

// completeWithTools runs the function-calling loop: the model may ask for
// server_shell, the command runs, and the output is fed back until the model
// produces a plain answer (or the round cap is hit).
func (a *HTTPAgent) completeWithTools(ctx context.Context, messages []ChatMessage) (string, error) {
	if a.shell == nil {
		return a.complete(ctx, messages)
	}
	msgs := append([]ChatMessage(nil), messages...)
	for round := 0; round < maxToolRounds; round++ {
		res, err := a.completeOnce(ctx, msgs, true)
		if err != nil {
			// Some providers 400 on the tools field itself. Keep chat working by
			// dropping the tool loop for this turn instead of failing the reply.
			if round == 0 && (res.status == http.StatusBadRequest || res.status == http.StatusUnprocessableEntity) {
				log.Printf("[shell] provider rejected tools (HTTP %d), replying without server_shell", res.status)
				return a.complete(ctx, messages)
			}
			return "", err
		}
		if len(res.toolCalls) == 0 {
			return res.content, nil
		}
		msgs = append(msgs, ChatMessage{Role: "assistant", Content: res.content, ToolCalls: res.toolCalls})
		for _, tc := range res.toolCalls {
			msgs = append(msgs, ChatMessage{Role: "tool", ToolCallID: tc.ID, Content: a.runTool(ctx, tc)})
		}
		log.Printf("[shell] tool round %d finished (%d call(s))", round+1, len(res.toolCalls))
	}
	return "", fmt.Errorf("工具调用轮数超过上限（%d）", maxToolRounds)
}

func (a *HTTPAgent) runTool(ctx context.Context, tc ToolCall) string {
	if tc.Function.Name != serverShellToolName {
		return "未知工具：" + tc.Function.Name
	}
	var args struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
		return "命令参数解析失败：" + err.Error()
	}
	log.Printf("[shell] exec: %s", truncateForLog(args.Command, 300))
	out, err := a.shell.Run(ctx, args.Command)
	if err != nil {
		if strings.TrimSpace(out) == "" {
			return err.Error()
		}
		return out + "\n[" + err.Error() + "]"
	}
	return out
}

func (a *HTTPAgent) doComplete(ctx context.Context, data []byte) (completion, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.endpoint, bytes.NewReader(data))
	if err != nil {
		return completion{}, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if a.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+a.apiKey)
	}
	for k, v := range a.headers {
		req.Header.Set(k, v)
	}

	resp, err := a.httpClient.Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return completion{}, fmt.Errorf("模型响应超时，请稍后再试")
		}
		return completion{}, fmt.Errorf("HTTP request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return completion{status: resp.StatusCode}, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return completion{status: resp.StatusCode, body: string(body)}, fmt.Errorf("API error HTTP %d", resp.StatusCode)
	}

	var result struct {
		Choices []struct {
			Message struct {
				Content   string     `json:"content"`
				ToolCalls []ToolCall `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return completion{}, fmt.Errorf("parse response: %w", err)
	}

	if len(result.Choices) == 0 {
		return completion{}, fmt.Errorf("no choices in response")
	}
	return completion{
		content:   result.Choices[0].Message.Content,
		toolCalls: result.Choices[0].Message.ToolCalls,
	}, nil
}

func retryableHTTPStatus(status int) bool {
	return status == http.StatusTooManyRequests || status >= 500
}

// friendlyHTTPError keeps provider JSON out of the chat window; the raw body
// still lands in the log for diagnosis.
func friendlyHTTPError(status int, body string) error {
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return fmt.Errorf("模型接口拒绝了请求（HTTP %d），检查 API Key 是否有效", status)
	case status == http.StatusTooManyRequests:
		return fmt.Errorf("模型接口限流了（HTTP 429），过一会儿再试")
	case status == http.StatusBadGateway, status == http.StatusServiceUnavailable, status == http.StatusGatewayTimeout:
		return fmt.Errorf("模型服务现在太忙（HTTP %d），请稍后再试", status)
	case status >= 500:
		return fmt.Errorf("模型服务出错（HTTP %d），请稍后再试", status)
	default:
		return fmt.Errorf("模型请求被拒（HTTP %d）：%s", status, truncateForLog(body, 120))
	}
}

func truncateForLog(s string, max int) string {
	s = strings.TrimSpace(strings.Join(strings.Fields(s), " "))
	r := []rune(s)
	if max <= 0 || len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}

func (a *HTTPAgent) chat(ctx context.Context, conversationID string, content any) (string, error) {
	a.mu.Lock()
	messages := a.buildMessages(conversationID, content)
	a.mu.Unlock()

	reply, err := a.completeWithTools(ctx, messages)
	if err != nil {
		return "", err
	}

	// Save to history
	a.mu.Lock()
	a.history[conversationID] = append(a.history[conversationID],
		ChatMessage{Role: "user", Content: content},
		ChatMessage{Role: "assistant", Content: reply},
	)
	// Trim history
	if len(a.history[conversationID]) > a.maxHistory*2 {
		a.history[conversationID] = a.history[conversationID][len(a.history[conversationID])-a.maxHistory*2:]
	}
	a.mu.Unlock()

	return reply, nil
}

func (a *HTTPAgent) buildMessages(conversationID string, content any) []ChatMessage {
	var messages []ChatMessage
	if a.systemPrompt != "" {
		messages = append(messages, ChatMessage{Role: "system", Content: a.systemPrompt})
	}
	if hist, ok := a.history[conversationID]; ok {
		messages = append(messages, hist...)
	}
	messages = append(messages, ChatMessage{Role: "user", Content: content})
	return messages
}

func buildNativeMediaContent(message string, attachments []Attachment) ([]chatContentPart, error) {
	parts := make([]chatContentPart, 0, len(attachments)+1)
	if strings.TrimSpace(message) != "" {
		parts = append(parts, chatContentPart{Type: "text", Text: message})
	}

	for _, attachment := range attachments {
		mediaType := attachment.MIMEType
		if mediaType == "" {
			mediaType = mime.TypeByExtension(filepath.Ext(attachment.Path))
		}
		mediaType = strings.ToLower(strings.TrimSpace(strings.Split(mediaType, ";")[0]))
		if !strings.HasPrefix(mediaType, "image/") && !strings.HasPrefix(mediaType, "video/") {
			return nil, fmt.Errorf("unsupported native media type %q", mediaType)
		}

		data, err := os.ReadFile(attachment.Path)
		if err != nil {
			return nil, fmt.Errorf("read attachment %q: %w", attachment.Path, err)
		}
		media := &chatMediaURL{URL: "data:" + mediaType + ";base64," + base64.StdEncoding.EncodeToString(data)}
		part := chatContentPart{Type: "image_url", ImageURL: media}
		if strings.HasPrefix(mediaType, "video/") {
			part = chatContentPart{Type: "video_url", VideoURL: media}
		}
		parts = append(parts, part)
	}

	return parts, nil
}
