package cmd

import (
	"testing"

	"github.com/fastclaw-ai/weclaw/config"
)

func TestSanitizeKey(t *testing.T) {
	cases := map[string]string{
		"  sk-abc\n": "sk-abc",
		`"sk-abc"`:   "sk-abc",
		"'sk-x'":     "sk-x",
		"  ":         "",
	}
	for in, want := range cases {
		if got := sanitizeKey(in); got != want {
			t.Fatalf("sanitizeKey(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestHasChatAgent(t *testing.T) {
	cases := []struct {
		name string
		cfg  *config.Config
		want bool
	}{
		{"empty", &config.Config{}, false},
		{"http with key", &config.Config{Agents: map[string]config.AgentConfig{
			"gemini": {Type: "http", Endpoint: "https://example.com", APIKey: "k"},
		}}, true},
		{"http without key", &config.Config{Agents: map[string]config.AgentConfig{
			"gemini": {Type: "http", Endpoint: "https://example.com"},
		}}, false},
		{"acp with command", &config.Config{Agents: map[string]config.AgentConfig{
			"claude": {Type: "acp", Command: "claude-agent-acp"},
		}}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := hasChatAgent(tc.cfg); got != tc.want {
				t.Fatalf("hasChatAgent = %v, want %v", got, tc.want)
			}
		})
	}
}
