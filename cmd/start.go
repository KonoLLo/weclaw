package cmd

import (
	"context"
	"fmt"
	"log"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/fastclaw-ai/weclaw/agent"
	"github.com/fastclaw-ai/weclaw/api"
	"github.com/fastclaw-ai/weclaw/config"
	"github.com/fastclaw-ai/weclaw/ilink"
	"github.com/fastclaw-ai/weclaw/messaging"
	"github.com/mdp/qrterminal/v3"
	"github.com/spf13/cobra"
)

var (
	foregroundFlag bool
	apiAddrFlag    string
)

func init() {
	startCmd.Flags().BoolVarP(&foregroundFlag, "foreground", "f", false, "Run in foreground (default is background)")
	startCmd.Flags().StringVar(&apiAddrFlag, "api-addr", "", "API server listen address (default 127.0.0.1:18011)")
	rootCmd.AddCommand(startCmd)
}

var startCmd = &cobra.Command{
	Use:   "start",
	Short: "Start the WeChat message bridge (auto-login if needed)",
	RunE:  runStart,
}

func runStart(cmd *cobra.Command, args []string) error {
	if !foregroundFlag {
		// Check if login is needed — if so, do it in foreground first, then daemon
		accounts, _ := ilink.LoadAllCredentials()
		if len(accounts) == 0 {
			fmt.Println("No WeChat accounts found, starting login...")
			ctx, cancel := notifyContext(context.Background())
			_, err := doLogin(ctx)
			cancel()
			if err != nil {
				return fmt.Errorf("login failed: %w", err)
			}
		}
		return runDaemon()
	}

	ctx, cancel := notifyContext(context.Background())
	defer cancel()

	// Load all accounts
	accounts, err := ilink.LoadAllCredentials()
	if err != nil {
		return fmt.Errorf("failed to load credentials: %w", err)
	}

	// No accounts — trigger login
	if len(accounts) == 0 {
		log.Println("No WeChat accounts found, starting login...")
		creds, err := doLogin(ctx)
		if err != nil {
			return fmt.Errorf("login failed: %w", err)
		}
		accounts = append(accounts, creds)
	}

	// Load config and auto-detect agents
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	ilink.ConfigureChannel(cfg.Channel.BotAgent, cfg.Channel.RouteTag)
	ilink.ConfigureInbound(cfg.Inbound.DebounceMS, cfg.Inbound.MaxBatch)
	messaging.ConfigureMediaDownload(cfg.Channel.MediaDownload.TimeoutMS, cfg.Channel.MediaDownload.IdleTimeoutMS)
	messaging.ConfigureErrorNotices(cfg.Channel.ErrorNotices)

	if config.DetectAndConfigure(cfg) {
		if err := config.Save(cfg); err != nil {
			log.Printf("Warning: failed to save auto-detected config: %v", err)
		} else {
			path, _ := config.ConfigPath()
			log.Printf("Auto-detected agents saved to %s", path)
		}
	}

	// Log all available agents
	if len(cfg.Agents) > 0 {
		names := make([]string, 0, len(cfg.Agents))
		for name := range cfg.Agents {
			names = append(names, name)
		}
		log.Printf("Available agents: %v (default: %s)", names, cfg.DefaultAgent)
	}

	// Create handler with an agent factory for on-demand agent creation
	handler := messaging.NewHandler(
		func(ctx context.Context, name string) agent.Agent {
			return createAgentByName(ctx, cfg, name)
		},
		func(name string) error {
			cfg.DefaultAgent = name
			return config.Save(cfg)
		},
	)

	memoryPath, err := messaging.DefaultMemoryStorePath()
	if err != nil {
		log.Printf("Warning: failed to resolve memory store path: %v", err)
	} else if memoryStore, err := messaging.NewMemoryStore(memoryPath); err != nil {
		log.Printf("Warning: failed to load memory store: %v", err)
	} else {
		handler.SetMemoryStore(memoryStore)
		log.Printf("Memory store: %s", memoryPath)
	}

	if img := newImageGen(cfg); img != nil {
		handler.SetImageGen(img)
	}
	if vg := newVideoGen(cfg); vg != nil {
		handler.SetVideoGen(vg)
	}
	if ws := newWebSearch(cfg); ws != nil {
		handler.SetWebSearch(ws)
	}
	handler.SetAutoSearch(!cfg.Search.DisableAuto)
	if tts := newTTS(); tts != nil {
		handler.SetTTS(tts)
	}
	if ws, err := newWorkspaceManager(cfg); err != nil {
		log.Printf("[workspace] disabled: %v", err)
	} else if ws != nil {
		handler.SetWorkspaceManager(ws)
		defer ws.Close()
	}

	// Populate agent metas for /status
	var metas []messaging.AgentMeta
	workDirs := make(map[string]string, len(cfg.Agents))
	for name, agCfg := range cfg.Agents {
		command := agCfg.Command
		if agCfg.Type == "http" {
			command = agCfg.Endpoint
		}
		metas = append(metas, messaging.AgentMeta{
			Name:    name,
			Type:    agCfg.Type,
			Command: command,
			Model:   agCfg.Model,
		})
		if agCfg.Cwd != "" {
			workDirs[name] = agCfg.Cwd
		}
	}
	handler.SetAgentMetas(metas)
	handler.SetAgentWorkDirs(workDirs)

	// Load custom aliases from agent configs
	handler.SetCustomAliases(config.BuildAliasMap(cfg.Agents))

	// Set save directory for images/files if configured
	if cfg.SaveDir != "" {
		handler.SetSaveDir(cfg.SaveDir)
		log.Printf("Image save directory: %s", cfg.SaveDir)
	}

	// Initialize cron scheduler
	cronStorePath, err := messaging.DefaultCronStorePath()
	if err != nil {
		log.Printf("Warning: failed to resolve cron store path: %v", err)
	} else {
		cronStore := messaging.NewCronStore(cronStorePath)
		if err := cronStore.Load(); err != nil {
			log.Printf("Warning: failed to load cron store: %v", err)
		}
		if len(cfg.AllowedUsers) > 0 {
			handler.SetAllowedUsers(cfg.AllowedUsers)
			log.Printf("Owner allowlist active: %d user(s)", len(cfg.AllowedUsers))
		} else {
			log.Println("Owner allowlist empty - serving any WeChat user")
		}
		handler.SetCronStore(cronStore)

		// Use the first account's client for sending cron messages
		firstClient := ilink.NewClient(accounts[0])
		targetUser := cfg.Heartbeat.TargetUser

		cronScheduler := messaging.NewCronScheduler(cronStore, firstClient, targetUser, func(name string) agent.Agent {
			if name == "" {
				return handler.GetDefaultAgent()
			}
			ag, err := handler.GetAgent(ctx, name)
			if err != nil {
				return nil
			}
			return ag
		})
		go cronScheduler.Start(ctx)

		// Start heartbeat runner
		if cfg.Heartbeat.Enabled {
			hbRunner, err := messaging.NewHeartbeatRunner(cfg.Heartbeat, firstClient, targetUser, handler.GetDefaultAgent)
			if err != nil {
				log.Printf("Warning: failed to start heartbeat runner: %v", err)
			} else {
				go hbRunner.Start(ctx)
			}
		}
	}

	// Start default agent initialization in background so monitors can start immediately.
	// Capture the configured name now so later config mutations do not register the
	// started agent under the wrong key.
	initialDefaultAgent := cfg.DefaultAgent
	go func() {
		if initialDefaultAgent == "" {
			log.Println("No default agent configured, staying in echo mode")
			return
		}
		log.Printf("Initializing default agent %q in background...", initialDefaultAgent)
		ag := createAgentByName(ctx, cfg, initialDefaultAgent)
		if ag == nil {
			log.Printf("Failed to initialize default agent %q, staying in echo mode", initialDefaultAgent)
		} else {
			handler.SetDefaultAgent(initialDefaultAgent, ag)
		}
	}()

	// Start HTTP API server for sending messages
	var clients []*ilink.Client
	for _, c := range accounts {
		clients = append(clients, ilink.NewClient(c))
	}
	// Resolve API addr: flag > env/config > default
	apiAddr := cfg.APIAddr // already includes env override from loadEnv
	if apiAddrFlag != "" {
		apiAddr = apiAddrFlag
	}
	apiServer := api.NewServer(clients, apiAddr)
	apiServer.SetContextTokens(handler.ContextTokens())
	go func() {
		if err := apiServer.Run(ctx); err != nil {
			log.Printf("API server error: %v", err)
		}
	}()

	for _, c := range clients {
		if err := c.NotifyStart(ctx); err != nil {
			log.Printf("[%s] notify start failed (advisory): %v", c.BotID(), err)
		}
	}
	defer func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		for _, c := range clients {
			if err := c.NotifyStop(stopCtx); err != nil {
				log.Printf("[%s] notify stop failed (advisory): %v", c.BotID(), err)
			}
		}
	}()

	// Start monitors immediately — they will use echo mode until agent is ready
	log.Printf("Starting message bridge for %d account(s)...", len(accounts))

	var wg sync.WaitGroup
	for _, creds := range accounts {
		wg.Add(1)
		go func(c *ilink.Credentials) {
			defer wg.Done()
			runMonitorWithRestart(ctx, c, handler)
		}(creds)
	}

	wg.Wait()
	log.Println("All monitors stopped")
	return nil
}

// runMonitorWithRestart runs a monitor with automatic restart on failure.
func runMonitorWithRestart(ctx context.Context, creds *ilink.Credentials, handler *messaging.Handler) {
	const maxRestartDelay = 30 * time.Second
	restartDelay := 3 * time.Second

	for {
		log.Printf("[%s] Starting monitor...", creds.ILinkBotID)

		client := ilink.NewClient(creds)
		monitor, err := ilink.NewMonitorWithBatch(client, handler.HandleMessages)
		if err != nil {
			log.Printf("[%s] Failed to create monitor: %v", creds.ILinkBotID, err)
		} else {
			monitor.SetUrgentHandler(handler.HandleUrgentMessage)
			err = monitor.Run(ctx)
		}

		// If context is cancelled, exit
		if ctx.Err() != nil {
			return
		}

		log.Printf("[%s] Monitor stopped: %v, restarting in %s", creds.ILinkBotID, err, restartDelay)
		select {
		case <-time.After(restartDelay):
		case <-ctx.Done():
			return
		}

		// Exponential backoff for restarts, capped
		restartDelay *= 2
		if restartDelay > maxRestartDelay {
			restartDelay = maxRestartDelay
		}
	}
}

// newWebSearch builds the switchable search table. DuckDuckGo needs no key;
// Tavily is listed but unusable until a key exists, so /se can say why instead
// of hiding the option.
func newWebSearch(cfg *config.Config) agent.Searcher {
	providers := []agent.SearchProvider{
		{Name: "DuckDuckGo", Searcher: agent.NewDuckDuckGo("", cfg.Search.MaxResults)},
	}
	key := cfg.Search.APIKey
	if key == "" {
		key = os.Getenv("TAVILY_API_KEY")
	}
	tavily := agent.SearchProvider{Name: "Tavily"}
	if key == "" {
		tavily.Unavailable = "没配 api_key"
	} else {
		tavily.Searcher = agent.NewWebSearch(key, cfg.Search.Endpoint, cfg.Search.MaxResults)
	}
	providers = append(providers, tavily)
	braveKey := strings.TrimSpace(cfg.Search.BraveKey)
	if braveKey == "" {
		braveKey = os.Getenv("BRAVE_API_KEY")
	}
	brave := agent.SearchProvider{Name: "Brave"}
	if braveKey == "" {
		brave.Unavailable = "没配 brave_api_key"
	} else {
		brave.Searcher = agent.NewBraveSearch(braveKey, "", cfg.Search.MaxResults)
	}
	providers = append(providers, brave)
	pool := agent.NewSearchPool(providers, cfg.Search.Provider)
	log.Printf("[search] current engine: %s", pool.Name())
	return pool
}

// endpointHost normalizes a URL to its lowercase hostname for key matching.
func endpointHost(endpoint string) string {
	u, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

// keyForGeneratorEndpoint picks an http agent key that belongs to the same
// provider as the generator endpoint. Zhipu's image/video APIs share the GLM
// agent's key, while the default agent (Gemini) key is rejected with 1000.
// Same-host agents win; the default agent key stays as the legacy fallback.
func keyForGeneratorEndpoint(cfg *config.Config, endpoint string) string {
	host := endpointHost(endpoint)
	if host != "" {
		if a, ok := cfg.Agents[cfg.DefaultAgent]; ok && a.Type == "http" && a.APIKey != "" && endpointHost(a.Endpoint) == host {
			return a.APIKey
		}
		names := make([]string, 0, len(cfg.Agents))
		for name := range cfg.Agents {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			a := cfg.Agents[name]
			if a.Type == "http" && a.APIKey != "" && endpointHost(a.Endpoint) == host {
				return a.APIKey
			}
		}
	}
	if a, ok := cfg.Agents[cfg.DefaultAgent]; ok && a.Type == "http" {
		return a.APIKey
	}
	return ""
}

// newImageGen builds the text-to-image generator. The key comes from the
// image section, else an agent on the same provider host, else the default
// http agent's key, else ZHIPU_API_KEY; with none of those the feature stays
// off rather than failing every draw.
func newImageGen(cfg *config.Config) *agent.ImageGen {
	ic := cfg.Image
	key := ic.APIKey
	if key == "" {
		endpoint := ic.Endpoint
		if endpoint == "" {
			endpoint = agent.DefaultImageEndpoint
		}
		key = keyForGeneratorEndpoint(cfg, endpoint)
	}
	if key == "" {
		key = os.Getenv("ZHIPU_API_KEY")
	}
	if key == "" {
		log.Printf("[image] no api_key; /image disabled")
		return nil
	}
	timeout := time.Duration(ic.TimeoutSeconds) * time.Second
	g := agent.NewImageGen(ic.Endpoint, key, ic.Model, timeout)
	g.SetSize(ic.Size)
	log.Printf("[image] text-to-image ready: model=%s size=%s", g.Model(), g.Size())
	return g
}

// newVideoGen builds the text-to-video generator. The key comes from the video
// section, else the image section, else an agent on the same provider host,
// else the default http agent's key, else ZHIPU_API_KEY; with none of those
// /视频 stays off.
func newVideoGen(cfg *config.Config) *agent.VideoGen {
	vc := cfg.Video
	key := vc.APIKey
	if key == "" {
		key = cfg.Image.APIKey
	}
	if key == "" {
		endpoint := vc.Endpoint
		if endpoint == "" {
			endpoint = agent.DefaultVideoEndpoint
		}
		key = keyForGeneratorEndpoint(cfg, endpoint)
	}
	if key == "" {
		key = os.Getenv("ZHIPU_API_KEY")
	}
	if key == "" {
		log.Printf("[video] no api_key; /视频 disabled")
		return nil
	}
	timeout := time.Duration(vc.TimeoutSeconds) * time.Second
	g := agent.NewVideoGen(vc.Endpoint, key, vc.Model, timeout)
	g.SetSize(vc.Size)
	g.SetQuality(vc.Quality)
	g.SetWithAudio(vc.WithAudio)
	log.Printf("[video] text-to-video ready: model=%s size=%s quality=%s", g.Model(), g.Size(), g.Quality())
	return g
}

func newWorkspaceManager(cfg *config.Config) (*messaging.WorkspaceManager, error) {
	if cfg.Workspace.Disabled {
		log.Printf("[workspace] disabled by config")
		return nil, nil
	}
	agentName := strings.TrimSpace(cfg.Workspace.Agent)
	if agentName != "" {
		agCfg, ok := cfg.Agents[agentName]
		if !ok || !workspaceAgentType(agCfg.Type) {
			log.Printf("[workspace] configured agent %q is missing or not acp/cli; /code disabled", agentName)
			agentName = ""
		}
	} else {
		if agCfg, ok := cfg.Agents["qwen"]; ok && workspaceAgentType(agCfg.Type) {
			agentName = "qwen"
		} else if agCfg, ok := cfg.Agents[cfg.DefaultAgent]; ok && workspaceAgentType(agCfg.Type) {
			agentName = cfg.DefaultAgent
		}
	}
	if agentName == "" {
		log.Printf("[workspace] no acp/cli agent available; /code disabled, file and run commands stay available")
	}

	opts := messaging.WorkspaceOptions{
		Enabled:         true,
		Root:            cfg.Workspace.Root,
		Agent:           agentName,
		AllowShell:      cfg.Workspace.AllowShell,
		Timeout:         time.Duration(cfg.Workspace.TimeoutSeconds) * time.Second,
		MaxOutputBytes:  cfg.Workspace.MaxOutputKB << 10,
		MaxArchiveBytes: int64(cfg.Workspace.MaxArchiveMB) << 20,
	}
	factory := func(ctx context.Context, name, cwd string) (agent.Agent, error) {
		ag := createAgentByNameWithCwd(ctx, cfg, name, cwd)
		if ag == nil {
			return nil, fmt.Errorf("agent %q failed to start", name)
		}
		return ag, nil
	}
	return messaging.NewWorkspaceManager(opts, factory)
}

func workspaceAgentType(t string) bool {
	return t == "acp" || t == "cli"
}

// createAgentByName creates and starts an agent by its config name.
// Returns nil if the agent is not configured or fails to start.
func createAgentByName(ctx context.Context, cfg *config.Config, name string) agent.Agent {
	return createAgentByNameWithCwd(ctx, cfg, name, "")
}

func createAgentByNameWithCwd(ctx context.Context, cfg *config.Config, name, cwd string) agent.Agent {
	agCfg, ok := cfg.Agents[name]
	if !ok {
		log.Printf("[agent] %q not found in config", name)
		return nil
	}
	workDir := agCfg.Cwd
	if cwd != "" {
		workDir = cwd
	}

	switch agCfg.Type {
	case "acp":
		ag := agent.NewACPAgent(agent.ACPAgentConfig{
			Command:      agCfg.Command,
			Args:         agCfg.Args,
			Cwd:          workDir,
			Env:          agCfg.Env,
			Model:        agCfg.Model,
			SystemPrompt: agCfg.SystemPrompt,
		})
		if err := ag.Start(ctx); err != nil {
			log.Printf("[agent] failed to start ACP agent %q: %v", name, err)
			return nil
		}
		log.Printf("[agent] started ACP agent: %s (command=%s, type=%s, model=%s)", name, agCfg.Command, agCfg.Type, agCfg.Model)
		return ag
	case "cli":
		ag := agent.NewCLIAgent(agent.CLIAgentConfig{
			Name:         name,
			Command:      agCfg.Command,
			Args:         agCfg.Args,
			Cwd:          workDir,
			Env:          agCfg.Env,
			Model:        agCfg.Model,
			SystemPrompt: agCfg.SystemPrompt,
		})
		log.Printf("[agent] created CLI agent: %s (command=%s, type=%s, model=%s)", name, agCfg.Command, agCfg.Type, agCfg.Model)
		return ag
	case "http":
		if agCfg.Endpoint == "" {
			log.Printf("[agent] HTTP agent %q has no endpoint", name)
			return nil
		}
		ag := agent.NewHTTPAgent(agent.HTTPAgentConfig{
			Endpoint:     agCfg.Endpoint,
			APIKey:       agCfg.APIKey,
			Headers:      agCfg.Headers,
			Model:        agCfg.Model,
			SystemPrompt: agCfg.SystemPrompt,
			MaxHistory:   agCfg.MaxHistory,
			Models:       agCfg.Models,
			Name:         name,
			Timeout:      time.Duration(agCfg.TimeoutSeconds) * time.Second,
		})
		log.Printf("[agent] created HTTP agent: %s (endpoint=%s, model=%s)", name, agCfg.Endpoint, agCfg.Model)
		return ag
	default:
		log.Printf("[agent] unknown type %q for %q", agCfg.Type, name)
		return nil
	}
}

// doLogin runs the interactive QR login flow and returns credentials.
func doLogin(ctx context.Context) (*ilink.Credentials, error) {
	fmt.Println("Fetching QR code...")
	qr, err := ilink.FetchQRCode(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch QR code: %w", err)
	}

	fmt.Println("\nScan this QR code with WeChat:")
	fmt.Println()
	qrterminal.GenerateWithConfig(qr.QRCodeImgContent, qrterminal.Config{
		Level:          qrterminal.L,
		Writer:         os.Stdout,
		HalfBlocks:     true,
		BlackChar:      qrterminal.BLACK_BLACK,
		WhiteBlackChar: qrterminal.WHITE_BLACK,
		WhiteChar:      qrterminal.WHITE_WHITE,
		BlackWhiteChar: qrterminal.BLACK_WHITE,
		QuietZone:      1,
	})
	fmt.Printf("\nQR URL: %s\n", qr.QRCodeImgContent)
	fmt.Println("\nWaiting for scan...")

	lastStatus := ""
	creds, err := ilink.PollQRStatus(ctx, qr.QRCode, func(status string) {
		if status != lastStatus {
			lastStatus = status
			switch status {
			case "scaned":
				fmt.Println("QR code scanned! Please confirm on your phone.")
			case "confirmed":
				fmt.Println("Login confirmed!")
			case "expired":
				fmt.Println("QR code expired.")
			}
		}
	})
	if err != nil {
		return nil, err
	}

	if err := ilink.SaveCredentials(creds); err != nil {
		return nil, fmt.Errorf("failed to save credentials: %w", err)
	}

	dir, _ := ilink.CredentialsPath()
	fmt.Printf("\nLogin successful! Credentials saved to %s\n", dir)
	fmt.Printf("Bot ID: %s\n\n", creds.ILinkBotID)
	return creds, nil
}

// --- Daemon mode ---

func weclawDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".weclaw")
}

func pidFile() string {
	return filepath.Join(weclawDir(), "weclaw.pid")
}

func logFile() string {
	return filepath.Join(weclawDir(), "weclaw.log")
}

// runDaemon spawns weclaw start (without --daemon) as a background process.
func runDaemon() error {
	// Kill any existing weclaw processes before starting a new one
	stopAllWeclaw()

	// Ensure log directory exists
	if err := os.MkdirAll(weclawDir(), 0o700); err != nil {
		return fmt.Errorf("create weclaw dir: %w", err)
	}

	// Open log file
	lf, err := os.OpenFile(logFile(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("open log file: %w", err)
	}

	// Re-exec ourselves without --daemon
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("find executable: %w", err)
	}

	cmd := exec.Command(exe, "start", "-f")
	cmd.Stdout = lf
	cmd.Stderr = lf
	setSysProcAttr(cmd)

	if err := cmd.Start(); err != nil {
		lf.Close()
		return fmt.Errorf("start daemon: %w", err)
	}

	// Save PID
	pid := cmd.Process.Pid
	os.WriteFile(pidFile(), []byte(fmt.Sprintf("%d", pid)), 0o644)

	// Detach — don't wait
	cmd.Process.Release()
	lf.Close()

	fmt.Printf("weclaw started in background (pid=%d)\n", pid)
	fmt.Printf("Log: %s\n", logFile())
	fmt.Printf("Stop: weclaw stop\n")
	return nil
}

func readPid() (int, error) {
	data, err := os.ReadFile(pidFile())
	if err != nil {
		return 0, err
	}
	var pid int
	if _, err := fmt.Sscanf(string(data), "%d", &pid); err != nil {
		return 0, err
	}
	return pid, nil
}

// stopAllWeclaw kills all running weclaw processes (by PID file and by process scan).
func stopAllWeclaw() {
	// 1. Kill by PID file
	if pid, err := readPid(); err == nil && processExists(pid) {
		if p, err := os.FindProcess(pid); err == nil {
			_ = signalTerminate(p)
		}
	}
	os.Remove(pidFile())

	// 2. Kill any remaining weclaw processes by scanning
	exe, err := os.Executable()
	if err != nil {
		return
	}
	killByName(exe)
	time.Sleep(500 * time.Millisecond)
}

func newTTS() *agent.TTS {
	t := agent.NewTTS("")
	if !t.Available() {
		log.Printf("[tts] edge-tts not found; voice disabled")
		return nil
	}
	log.Printf("[tts] edge-tts ready: default=%s", t.Voice())
	return t
}
