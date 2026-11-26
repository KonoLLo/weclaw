package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// ModelProbe is the outcome of one availability check against a candidate model.
type ModelProbe struct {
	Model   string
	Status  string
	Detail  string
	Elapsed time.Duration
}

// probePerModelTimeout caps one candidate: a congested model must not hold
// up the whole table.
const probePerModelTimeout = 12 * time.Second

// modelStatePath persists the per-agent model choice so a restart does not
// silently drop back to the configured default.
func modelStatePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".weclaw", "models.json"), nil
}

func loadModelSelection(agentName string) string {
	p, err := modelStatePath()
	if err != nil {
		return ""
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	var m map[string]string
	if json.Unmarshal(b, &m) != nil {
		return ""
	}
	return m[agentName]
}

func saveModelSelection(agentName, model string) {
	p, err := modelStatePath()
	if err != nil {
		return
	}
	m := map[string]string{}
	if b, err := os.ReadFile(p); err == nil {
		json.Unmarshal(b, &m)
	}
	m[agentName] = model
	if b, err := json.MarshalIndent(m, "", " "); err == nil {
		os.MkdirAll(filepath.Dir(p), 0o700)
		os.WriteFile(p, b, 0o600)
	}
}

// Models lists the candidate pool; empty means this agent has no pool.
func (a *HTTPAgent) Models() []string { return a.models }

// CurrentModel is the model actually used for the next request.
func (a *HTTPAgent) CurrentModel() string { return a.currentModel() }

// SetModel switches the pool to the named model and persists the choice.
// An empty argument means "whatever is currently selected".
func (a *HTTPAgent) SetModel(name string) error {
	if name == "" {
		return fmt.Errorf("empty model name")
	}
	if len(a.models) > 0 {
		found := false
		for _, m := range a.models {
			if strings.EqualFold(m, name) {
				found = true
				name = m
				break
			}
		}
		if !found {
			return fmt.Errorf("model %q is not in the pool", name)
		}
	}
	a.mu.Lock()
	a.model = name
	a.mu.Unlock()
	saveModelSelection(a.stateKey, name)
	return nil
}

func (a *HTTPAgent) currentModel() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.model
}

// ProbeModels calls every candidate once with a minimal request and reports
// what the provider actually answered. Availability on free tiers fluctuates
// minute to minute, so a result here is a snapshot, not a verdict.
func (a *HTTPAgent) ProbeModels(ctx context.Context) []ModelProbe {
	a.mu.Lock()
	list := append([]string(nil), a.models...)
	if len(list) == 0 {
		list = []string{a.model}
	}
	a.mu.Unlock()

	out := make([]ModelProbe, len(list))
	var wg sync.WaitGroup
	for i, m := range list {
		wg.Add(1)
		go func(i int, m string) {
			defer wg.Done()
			pctx, pcancel := context.WithTimeout(ctx, probePerModelTimeout)
			defer pcancel()
			out[i] = a.probeOne(pctx, m)
		}(i, m)
	}
	wg.Wait()
	return out
}

func (a *HTTPAgent) probeOne(ctx context.Context, model string) ModelProbe {
	body, _ := json.Marshal(map[string]any{
		"model":      model,
		"messages":   []map[string]string{{"role": "user", "content": "ping"}},
		"max_tokens": 1,
	})
	start := time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.endpoint, strings.NewReader(string(body)))
	if err != nil {
		return ModelProbe{Model: model, Status: "构造失败", Detail: err.Error(), Elapsed: time.Since(start)}
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
		st := "连不上"
		if errors.Is(err, context.DeadlineExceeded) {
			st = fmt.Sprintf("超时（>%ds）", int(probePerModelTimeout.Seconds()))
		}
		return ModelProbe{Model: model, Status: st, Detail: err.Error(), Elapsed: time.Since(start)}
	}
	defer resp.Body.Close()
	var b strings.Builder
	buf := make([]byte, 4096)
	n, _ := resp.Body.Read(buf)
	b.Write(buf[:n])
	snip := strings.TrimSpace(b.String())
	if len(snip) > 160 {
		snip = snip[:160]
	}
	st := resp.StatusCode
	switch {
	case st == 200:
		return ModelProbe{Model: model, Status: "可用", Elapsed: time.Since(start)}
	case strings.Contains(snip, "1305"):
		return ModelProbe{Model: model, Status: "暂时拥堵", Detail: snip, Elapsed: time.Since(start)}
	case strings.Contains(snip, "1113"):
		return ModelProbe{Model: model, Status: "无额度", Detail: snip, Elapsed: time.Since(start)}
	case st == 429:
		return ModelProbe{Model: model, Status: "限流", Detail: snip, Elapsed: time.Since(start)}
	case st == 401 || st == 403:
		return ModelProbe{Model: model, Status: "无权限", Detail: snip, Elapsed: time.Since(start)}
	case st == 404:
		return ModelProbe{Model: model, Status: "模型不存在", Detail: snip, Elapsed: time.Since(start)}
	default:
		return ModelProbe{Model: model, Status: fmt.Sprintf("HTTP %d", st), Detail: snip, Elapsed: time.Since(start)}
	}
}
