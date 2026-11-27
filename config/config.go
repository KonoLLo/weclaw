package config

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
)

// Config holds the application configuration.
type Config struct {
	DefaultAgent string                 `json:"default_agent"`
	APIAddr      string                 `json:"api_addr,omitempty"`
	SaveDir      string                 `json:"save_dir,omitempty"`
	AllowedUsers []string               `json:"allowed_users,omitempty"`
	Agents       map[string]AgentConfig `json:"agents"`
	Heartbeat    HeartbeatConfig        `json:"heartbeat,omitempty"`
	Image        ImageConfig            `json:"image,omitempty"`
	Video        VideoConfig            `json:"video,omitempty"`
	Search       SearchConfig           `json:"search,omitempty"`
	Inbound      InboundConfig          `json:"inbound,omitempty"`
	Channel      ChannelConfig          `json:"channel,omitempty"`
	Workspace    WorkspaceConfig        `json:"workspace,omitempty"`
}

// InboundConfig controls per-sender debounce and batch limits.
type InboundConfig struct {
	DebounceMS int `json:"debounce_ms,omitempty"` // 0 = 800ms
	MaxBatch   int `json:"max_batch,omitempty"`   // 0 = 8
}

// ChannelConfig holds optional iLink channel metadata and operational limits.
type ChannelConfig struct {
	BotAgent      string              `json:"bot_agent,omitempty"` // ASCII Name/Version, max 256 bytes
	RouteTag      int64               `json:"route_tag,omitempty"` // SKRouteTag / _cradle_hostname=g<tag>
	ErrorNotices  bool                `json:"error_notices,omitempty"`
	MediaDownload MediaDownloadConfig `json:"media_download,omitempty"`
}

// MediaDownloadConfig sets inbound media transfer limits.
type MediaDownloadConfig struct {
	TimeoutMS     int `json:"timeout_ms,omitempty"`      // 0 = 120000
	IdleTimeoutMS int `json:"idle_timeout_ms,omitempty"` // 0 = 30000
}

// WorkspaceConfig controls the per-user code workspace used by /code and /run.
// The feature is on by default; set disabled to turn all workspace commands off.
type WorkspaceConfig struct {
	Disabled       bool   `json:"disabled,omitempty"`
	Root           string `json:"root,omitempty"`  // default: ~/.weclaw/workspaces
	Agent          string `json:"agent,omitempty"` // default: qwen, then default agent
	AllowShell     bool   `json:"allow_shell,omitempty"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty"` // default: 600
	MaxOutputKB    int    `json:"max_output_kb,omitempty"`   // default: 64
	MaxArchiveMB   int    `json:"max_archive_mb,omitempty"`  // default: 50
}

// SearchConfig configures the switchable web-search clients. Provider is the
// startup default; a saved /se choice wins over it.
type SearchConfig struct {
	Provider    string `json:"provider,omitempty"` // "duckduckgo" (default) or "tavily"
	APIKey      string `json:"api_key,omitempty"`  // Tavily only
	BraveKey    string `json:"brave_api_key,omitempty"`
	DisableAuto bool   `json:"disable_auto,omitempty"` // true = never search before a normal reply
	Endpoint    string `json:"endpoint,omitempty"`
	MaxResults  int    `json:"max_results,omitempty"`
}

// ImageConfig configures the text-to-image generator used by /图 and the
// drawing keywords. Only free-tier models are wired in here; empty fields fall
// back to the defaults below and, for the key, to the default http agent's key.
type ImageConfig struct {
	Model          string `json:"model,omitempty"`           // default: cogview-3-flash
	Size           string `json:"size,omitempty"`            // default: 1024x1024
	Endpoint       string `json:"endpoint,omitempty"`        // default: Zhipu images/generations
	APIKey         string `json:"api_key,omitempty"`         // default: default http agent key
	TimeoutSeconds int    `json:"timeout_seconds,omitempty"` // 0 = 120s
}

// VideoConfig configures the text-to-video generator used by /视频. Only the
// free CogVideoX-Flash model is wired in; paid Vidu/CogVideoX tiers are not
// defaults anywhere. Empty fields fall back to the defaults in agent.NewVideoGen.
type VideoConfig struct {
	Model          string `json:"model,omitempty"`           // default: cogvideox-flash
	Size           string `json:"size,omitempty"`            // default: 1920x1080
	Quality        string `json:"quality,omitempty"`         // speed (default) or quality
	WithAudio      bool   `json:"with_audio,omitempty"`      // default false
	Endpoint       string `json:"endpoint,omitempty"`        // default: Zhipu videos/generations
	APIKey         string `json:"api_key,omitempty"`         // default: image key, then agent key
	TimeoutSeconds int    `json:"timeout_seconds,omitempty"` // 0 = 300s
}

// HeartbeatConfig holds heartbeat runner configuration.
type HeartbeatConfig struct {
	Enabled     bool   `json:"enabled"`
	Interval    string `json:"interval,omitempty"`     // e.g. "30m"
	ActiveHours string `json:"active_hours,omitempty"` // e.g. "09:00-18:00"
	Timezone    string `json:"timezone,omitempty"`     // IANA timezone
	TargetUser  string `json:"target_user,omitempty"`  // user ID to send heartbeat results to
}

// AgentConfig holds configuration for a single agent.
type AgentConfig struct {
	Type           string            `json:"type"`                      // "acp", "cli", or "http"
	Command        string            `json:"command,omitempty"`         // binary path (cli/acp type)
	Args           []string          `json:"args,omitempty"`            // extra args for command (e.g. ["acp"] for cursor)
	Aliases        []string          `json:"aliases,omitempty"`         // custom trigger commands (e.g. ["gpt", "4o"])
	Cwd            string            `json:"cwd,omitempty"`             // working directory (workspace)
	Env            map[string]string `json:"env,omitempty"`             // extra environment variables (cli/acp type)
	Model          string            `json:"model,omitempty"`           // model name
	SystemPrompt   string            `json:"system_prompt,omitempty"`   // system prompt
	Endpoint       string            `json:"endpoint,omitempty"`        // API endpoint (http type)
	APIKey         string            `json:"api_key,omitempty"`         // API key (http type)
	Headers        map[string]string `json:"headers,omitempty"`         // extra HTTP headers (http type)
	MaxHistory     int               `json:"max_history,omitempty"`     // max history (http type)
	Models         []string          `json:"models,omitempty"`          // candidate pool for /mod (http type)
	TimeoutSeconds int               `json:"timeout_seconds,omitempty"` // http agent request timeout (0 = default 300s)
}

// BuildAliasMap builds a map from custom alias to agent name from all agent configs.
// It logs warnings for conflicts: duplicate aliases and aliases shadowing agent keys.
func BuildAliasMap(agents map[string]AgentConfig) map[string]string {
	// Built-in commands that cannot be overridden
	reserved := map[string]bool{
		"info": true, "help": true, "new": true, "clear": true, "cwd": true,
	}

	m := make(map[string]string)
	for name, cfg := range agents {
		for _, alias := range cfg.Aliases {
			if reserved[alias] {
				log.Printf("[config] WARNING: alias %q for agent %q conflicts with built-in command, ignored", alias, name)
				continue
			}
			if existing, ok := m[alias]; ok {
				log.Printf("[config] WARNING: alias %q is defined by both %q and %q, using %q", alias, existing, name, name)
			}
			m[alias] = name
		}
	}

	// Warn if a custom alias shadows an agent key
	for alias, target := range m {
		if _, isAgent := agents[alias]; isAgent && alias != target {
			log.Printf("[config] WARNING: alias %q (-> %q) shadows agent key %q", alias, target, alias)
		}
	}

	return m
}

// DefaultConfig returns an empty configuration.
func DefaultConfig() *Config {
	return &Config{
		Agents: make(map[string]AgentConfig),
	}
}

// ConfigPath returns the path to the config file.
func ConfigPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".weclaw", "config.json"), nil
}

// Load loads configuration from disk and environment variables.
func Load() (*Config, error) {
	cfg := DefaultConfig()

	path, err := ConfigPath()
	if err != nil {
		return cfg, nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			loadEnv(cfg)
			return cfg, nil
		}
		return nil, fmt.Errorf("read config: %w", err)
	}

	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if cfg.Agents == nil {
		cfg.Agents = make(map[string]AgentConfig)
	}

	loadEnv(cfg)
	return cfg, nil
}

func loadEnv(cfg *Config) {
	if v := os.Getenv("WECLAW_DEFAULT_AGENT"); v != "" {
		cfg.DefaultAgent = v
	}
	if v := os.Getenv("WECLAW_API_ADDR"); v != "" {
		cfg.APIAddr = v
	}
	if v := os.Getenv("WECLAW_SAVE_DIR"); v != "" {
		cfg.SaveDir = v
	}
}

// Save saves the configuration to disk.
func Save(cfg *Config) error {
	path, err := ConfigPath()
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}

	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}

	return os.WriteFile(path, data, 0o600)
}
