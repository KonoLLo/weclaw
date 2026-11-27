package cmd

import (
	"testing"

	"github.com/fastclaw-ai/weclaw/agent"
	"github.com/fastclaw-ai/weclaw/config"
)

func TestKeyForGeneratorEndpoint(t *testing.T) {
	cfg := &config.Config{
		DefaultAgent: "gemini",
		Agents: map[string]config.AgentConfig{
			"gemini": {
				Type:     "http",
				Endpoint: "https://generativelanguage.googleapis.com/v1beta/openai/chat/completions",
				APIKey:   "google-key",
			},
			"glm": {
				Type:     "http",
				Endpoint: "https://open.bigmodel.cn/api/paas/v4/chat/completions",
				APIKey:   "zhipu-key",
			},
		},
	}
	// The Zhipu image endpoint must pick the GLM agent key, not Gemini's.
	if got := keyForGeneratorEndpoint(cfg, agent.DefaultImageEndpoint); got != "zhipu-key" {
		t.Fatalf("image endpoint key = %q, want zhipu-key", got)
	}
	if got := keyForGeneratorEndpoint(cfg, agent.DefaultVideoEndpoint); got != "zhipu-key" {
		t.Fatalf("video endpoint key = %q, want zhipu-key", got)
	}

	// When the default agent shares the host it wins deterministically.
	cfg.DefaultAgent = "glm"
	if got := keyForGeneratorEndpoint(cfg, agent.DefaultImageEndpoint); got != "zhipu-key" {
		t.Fatalf("default glm key = %q, want zhipu-key", got)
	}

	// No agent on that host: keep the legacy default-agent fallback.
	cfg.DefaultAgent = "gemini"
	if got := keyForGeneratorEndpoint(cfg, "https://example.com/v1/images"); got != "google-key" {
		t.Fatalf("legacy fallback = %q, want google-key", got)
	}

	// No usable agent at all.
	empty := &config.Config{DefaultAgent: "nope", Agents: map[string]config.AgentConfig{}}
	if got := keyForGeneratorEndpoint(empty, agent.DefaultImageEndpoint); got != "" {
		t.Fatalf("empty config key = %q, want empty", got)
	}
}
