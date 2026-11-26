package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Only free-tier Zhipu models are wired in here; paid ones (CogVideoX-2,
// CogVideoX-3, Vidu) are deliberately not defaults anywhere in this package.
const (
	DefaultVideoModel          = "cogvideox-flash"
	DefaultVideoEndpoint       = "https://open.bigmodel.cn/api/paas/v4/videos/generations"
	DefaultVideoResultEndpoint = "https://open.bigmodel.cn/api/paas/v4/async-result/"
	DefaultVideoSize           = "1920x1080"
	DefaultVideoTimeout        = 5 * time.Minute
	DefaultVideoPollInterval   = 5 * time.Second
)

// SupportedVideoSizes is the documented CogVideoX-Flash set; anything else the
// API rejects, so requests are snapped into this list.
var SupportedVideoSizes = []string{
	"720x480", "1024x1024", "1280x960", "960x1280",
	"1920x1080", "1080x1920", "2048x1080", "3840x2160",
}

// VideoGen submits an async CogVideoX task and polls until the mp4 URL exists.
type VideoGen struct {
	endpoint       string
	resultEndpoint string
	apiKey         string
	model          string
	size           string
	quality        string
	withAudio      bool
	timeout        time.Duration
	pollInterval   time.Duration
	client         *http.Client
}

func NewVideoGen(endpoint, apiKey, model string, timeout time.Duration) *VideoGen {
	if endpoint == "" {
		endpoint = DefaultVideoEndpoint
	}
	resultEndpoint := DefaultVideoResultEndpoint
	if strings.HasSuffix(endpoint, "/videos/generations") {
		resultEndpoint = strings.TrimSuffix(endpoint, "/videos/generations") + "/async-result/"
	}
	if model == "" {
		model = DefaultVideoModel
	}
	if timeout <= 0 {
		timeout = DefaultVideoTimeout
	}
	return &VideoGen{
		endpoint:       endpoint,
		resultEndpoint: resultEndpoint,
		apiKey:         apiKey,
		model:          model,
		size:           DefaultVideoSize,
		quality:        "speed",
		timeout:        timeout,
		pollInterval:   DefaultVideoPollInterval,
		client:         &http.Client{Timeout: 60 * time.Second},
	}
}

func (g *VideoGen) Model() string { return g.model }
func (g *VideoGen) Size() string  { return g.size }

// SetSize sets the default size for prompts that do not name one.
func (g *VideoGen) SetSize(size string) {
	if s := NormalizeVideoSize(size); s != "" {
		g.size = s
	}
}

func (g *VideoGen) SetQuality(quality string) {
	switch strings.ToLower(strings.TrimSpace(quality)) {
	case "quality", "speed":
		g.quality = strings.ToLower(strings.TrimSpace(quality))
	}
}

func (g *VideoGen) SetWithAudio(on bool) { g.withAudio = on }

func (g *VideoGen) Quality() string { return g.quality }

type videoAPIError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type videoResponse struct {
	ID          string `json:"id"`
	TaskStatus  string `json:"task_status"`
	VideoResult []struct {
		URL string `json:"url"`
	} `json:"video_result"`
	Error *videoAPIError `json:"error"`
}

// GenerateVideo returns the mp4 URL once the async task succeeds.
func (g *VideoGen) GenerateVideo(ctx context.Context, prompt, size string) (string, error) {
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return "", fmt.Errorf("提示词是空的")
	}
	if utf8.RuneCountInString(prompt) > 512 {
		return "", fmt.Errorf("提示词太长（最多 512 字）")
	}
	if size == "" {
		size = g.size
	} else if s := NormalizeVideoSize(size); s != "" {
		size = s
	} else {
		size = g.size
	}

	gctx, cancel := context.WithTimeout(ctx, g.timeout)
	defer cancel()

	payload := map[string]any{
		"model":  g.model,
		"prompt": prompt,
		"size":   size,
	}
	if g.quality != "" {
		payload["quality"] = g.quality
	}
	if g.withAudio {
		payload["with_audio"] = true
	}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(gctx, http.MethodPost, g.endpoint, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if g.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+g.apiKey)
	}
	resp, err := g.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("提交任务没发出去：%w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var sub videoResponse
	_ = json.Unmarshal(raw, &sub)
	if err := videoHTTPError(resp.StatusCode, raw, sub.Error); err != nil {
		return "", err
	}
	if sub.ID == "" {
		if u := firstVideoURL(sub); u != "" {
			return u, nil
		}
		return "", fmt.Errorf("提交后没拿到任务号：%s", trimSnippet(string(raw)))
	}
	log.Printf("[video] task %s submitted (%s %s)", sub.ID, g.model, size)

	ticker := time.NewTicker(g.pollInterval)
	defer ticker.Stop()
	var lastErr error
	lastStatus := strings.ToUpper(sub.TaskStatus)
	for {
		select {
		case <-gctx.Done():
			if lastErr != nil {
				return "", fmt.Errorf("等生成超时了，最后一次错误：%v", lastErr)
			}
			return "", fmt.Errorf("等生成超时了（%s），任务可能还在跑，稍后再试", g.timeout)
		case <-ticker.C:
			var out videoResponse
			outRaw, err := g.getResult(gctx, sub.ID, &out)
			status := strings.ToUpper(out.TaskStatus)
			if status != "" && status != lastStatus {
				log.Printf("[video] task %s status %s", sub.ID, status)
				lastStatus = status
			}
			if status == "SUCCESS" {
				if u := firstVideoURL(out); u != "" {
					log.Printf("[video] task %s ready", sub.ID)
					return u, nil
				}
				return "", fmt.Errorf("上游说成功但没给视频地址：%s", trimSnippet(string(outRaw)))
			}
			if status == "FAIL" {
				return "", fmt.Errorf("上游生成失败：%s", videoFailureDetail(out, outRaw))
			}
			if err != nil {
				// A structured API error is final; a network hiccup is not.
				if out.Error != nil {
					return "", fmt.Errorf("上游生成失败：%s", videoFailureDetail(out, outRaw))
				}
				lastErr = err
				continue
			}
			lastErr = nil
		}
	}
}

func (g *VideoGen) getResult(ctx context.Context, id string, out *videoResponse) ([]byte, error) {
	reqURL := strings.TrimRight(g.resultEndpoint, "/") + "/" + url.PathEscape(id)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, err
	}
	if g.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+g.apiKey)
	}
	resp, err := g.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("查询任务状态失败：%w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	_ = json.Unmarshal(raw, out)
	if err := videoHTTPError(resp.StatusCode, raw, out.Error); err != nil {
		return raw, err
	}
	return raw, nil
}

func videoHTTPError(status int, raw []byte, apiErr *videoAPIError) error {
	if apiErr != nil {
		return fmt.Errorf("上游说 %s：%s", apiErr.Code, trimSnippet(apiErr.Message))
	}
	if status != http.StatusOK {
		return fmt.Errorf("HTTP %d：%s", status, trimSnippet(string(raw)))
	}
	return nil
}

func firstVideoURL(r videoResponse) string {
	for _, v := range r.VideoResult {
		if u := strings.TrimSpace(v.URL); u != "" {
			return u
		}
	}
	return ""
}

func videoFailureDetail(r videoResponse, raw []byte) string {
	if r.Error != nil && (r.Error.Code != "" || r.Error.Message != "") {
		return trimSnippet(r.Error.Code + " " + r.Error.Message)
	}
	return trimSnippet(string(raw))
}

func NormalizeVideoSize(size string) string {
	size = strings.ToLower(strings.TrimSpace(size))
	size = strings.ReplaceAll(size, "×", "x")
	size = strings.ReplaceAll(size, "*", "x")
	parts := strings.Split(size, "x")
	if len(parts) != 2 {
		return ""
	}
	w, err1 := strconv.Atoi(strings.TrimSpace(parts[0]))
	h, err2 := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err1 != nil || err2 != nil || w <= 0 || h <= 0 {
		return ""
	}
	target := float64(w) / float64(h)
	best := ""
	bestAspect := math.MaxFloat64
	bestArea := math.MaxFloat64
	for _, s := range SupportedVideoSizes {
		sp := strings.Split(s, "x")
		sw, _ := strconv.Atoi(sp[0])
		sh, _ := strconv.Atoi(sp[1])
		aspect := math.Abs(float64(sw)/float64(sh) - target)
		area := math.Abs(float64(sw*sh-w*h)) / float64(sw*sh)
		if aspect < bestAspect-1e-9 || (math.Abs(aspect-bestAspect) <= 1e-9 && area < bestArea) {
			best, bestAspect, bestArea = s, aspect, area
		}
	}
	return best
}
