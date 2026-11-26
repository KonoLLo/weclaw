package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Only free-tier Zhipu models are wired in here; paid ones (CogView-4, Vidu)
// are deliberately not defaults anywhere in this package.
const (
	DefaultImageModel    = "cogview-3-flash"
	DefaultImageEndpoint = "https://open.bigmodel.cn/api/paas/v4/images/generations"
	DefaultImageTimeout  = 120 * time.Second
	DefaultImageSize     = "1024x1024"
)

// SupportedImageSizes is the documented CogView-3-Flash set; anything else the
// API rejects, so requests are snapped into this list.
var SupportedImageSizes = []string{
	"1024x1024", "768x1344", "864x1152", "1344x768", "1152x864", "1440x720", "720x1440",
}

// ImageGen turns a text prompt into a hosted image URL.
type ImageGen struct {
	endpoint string
	apiKey   string
	model    string
	size     string
	client   *http.Client
}

func NewImageGen(endpoint, apiKey, model string, timeout time.Duration) *ImageGen {
	if endpoint == "" {
		endpoint = DefaultImageEndpoint
	}
	if model == "" {
		model = DefaultImageModel
	}
	if timeout <= 0 {
		timeout = DefaultImageTimeout
	}
	return &ImageGen{endpoint: endpoint, apiKey: apiKey, model: model, size: DefaultImageSize, client: &http.Client{Timeout: timeout}}
}

func (g *ImageGen) Model() string { return g.model }

// SetSize sets the default size for prompts that do not name one.
func (g *ImageGen) SetSize(size string) {
	if s := NormalizeImageSize(size); s != "" {
		g.size = s
	}
}

func (g *ImageGen) Size() string { return g.size }

// GenerateImage returns the URL of the generated image.
func (g *ImageGen) GenerateImage(ctx context.Context, prompt, size string) (string, error) {
	if strings.TrimSpace(prompt) == "" {
		return "", fmt.Errorf("提示词是空的")
	}
	if size == "" {
		size = g.size
	} else {
		size = NormalizeImageSize(size)
	}
	body, _ := json.Marshal(map[string]string{"model": g.model, "prompt": prompt, "size": size})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.endpoint, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if g.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+g.apiKey)
	}
	resp, err := g.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("请求没发出去：%w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	var out struct {
		Data []struct {
			URL     string `json:"url"`
			B64JSON string `json:"b64_json"`
		} `json:"data"`
		Error *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(raw, &out)
	if out.Error != nil {
		return "", fmt.Errorf("上游说 %s：%s", out.Error.Code, trimSnippet(out.Error.Message))
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d：%s", resp.StatusCode, trimSnippet(string(raw)))
	}
	for _, d := range out.Data {
		if u := strings.TrimSpace(d.URL); u != "" {
			return u, nil
		}
	}
	return "", fmt.Errorf("上游返回 200 但图里没地址：%s", trimSnippet(string(raw)))
}

func trimSnippet(s string) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) > 140 {
		return string(r[:140]) + "…"
	}
	return s
}

// NormalizeImageSize returns the nearest documented size, or "" when the input
// cannot be parsed. Matching is by aspect ratio first, then by area.
func NormalizeImageSize(size string) string {
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
	for _, s := range SupportedImageSizes {
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
