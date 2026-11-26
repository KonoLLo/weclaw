package messaging

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fastclaw-ai/weclaw/agent"
	"github.com/fastclaw-ai/weclaw/ilink"
	"github.com/google/uuid"
)

// AgentFactory creates an agent by config name. Returns nil if the name is unknown.
type AgentFactory func(ctx context.Context, name string) agent.Agent

// SaveDefaultFunc persists the default agent name to config file.
type SaveDefaultFunc func(name string) error

// AgentMeta holds static config info about an agent (for /status display).
type AgentMeta struct {
	Name    string
	Type    string // "acp", "cli", "http"
	Command string // binary path or endpoint
	Model   string
}

// Handler processes incoming WeChat messages and dispatches replies.
type Handler struct {
	mu            sync.RWMutex
	defaultName   string
	agents        map[string]agent.Agent // name -> running agent
	agentMetas    []AgentMeta            // all configured agents (for /status)
	agentWorkDirs map[string]string      // agent name -> configured/runtime cwd
	customAliases map[string]string      // custom alias -> agent name (from config)
	factory       AgentFactory
	saveDefault   SaveDefaultFunc
	contextTokens sync.Map          // map[userID]contextToken
	saveDir       string            // directory to save images/files to
	seenMsgs      sync.Map          // map[int64]time.Time — dedup by message_id
	pendingFiles  sync.Map          // map[userID]pendingIncomingFile — last upload waiting for a question
	cronStore     *CronStore        // cron job store (nil if not configured)
	memoryStore   *MemoryStore      // long-term per-user notes (nil = disabled)
	allowedUsers  []string          // when non-empty, only these WeChat user IDs are served
	imageGen      *agent.ImageGen   // text-to-image generator (nil = /图 disabled)
	videoGen      *agent.VideoGen   // text-to-video generator (nil = /视频 disabled)
	webSearch     agent.Searcher    // DuckDuckGo or Tavily (nil = disabled)
	tts           *agent.TTS        // edge-tts client (nil = voice disabled)
	workspace     *WorkspaceManager // per-user code workspace (nil = disabled)

	tasksMu sync.Mutex
	tasks   map[string]map[uint64]runningTask
	taskSeq uint64
}

// ContextTokens exposes the per-user context-token cache (used by the
// outbound API so proactive pushes carry the token iLink requires).
func (h *Handler) ContextTokens() *sync.Map {
	return &h.contextTokens
}

func (h *Handler) defaultAgentUsesNativeCommands() bool {
	h.mu.RLock()
	defer h.mu.RUnlock()

	defaultName := strings.ToLower(strings.TrimSpace(h.defaultName))
	if strings.Contains(defaultName, "hermes") {
		return true
	}

	if ag, ok := h.agents[h.defaultName]; ok {
		info := ag.Info()
		if strings.Contains(strings.ToLower(filepath.Base(info.Command)), "hermes") {
			return true
		}
	}

	for _, meta := range h.agentMetas {
		if meta.Name != h.defaultName {
			continue
		}
		if strings.Contains(strings.ToLower(filepath.Base(meta.Command)), "hermes") {
			return true
		}
	}

	return false
}

func (h *Handler) shouldBypassBridgeCommand(trimmed string) bool {
	if !h.defaultAgentUsesNativeCommands() {
		return false
	}

	switch trimmed {
	case "/info", "/help", "/new", "/clear":
		return true
	default:
		return false
	}
}

// NewHandler creates a new message handler.
func NewHandler(factory AgentFactory, saveDefault SaveDefaultFunc) *Handler {
	return &Handler{
		agents:        make(map[string]agent.Agent),
		agentWorkDirs: make(map[string]string),
		factory:       factory,
		saveDefault:   saveDefault,
	}
}

// SetAllowedUsers restricts which WeChat users are served. An empty list keeps
// the upstream behaviour of serving anyone, so existing installs are unaffected.
func (h *Handler) SetAllowedUsers(users []string) { h.allowedUsers = users }

// SetImageGen enables /图 and the drawing keywords. Nil leaves them off.
func (h *Handler) SetImageGen(g *agent.ImageGen) { h.imageGen = g }

// SetVideoGen enables /视频 and the video keywords. Nil leaves them off.
func (h *Handler) SetVideoGen(g *agent.VideoGen) { h.videoGen = g }

// SetWebSearch enables /s and the search keywords.
func (h *Handler) SetWebSearch(w agent.Searcher) { h.webSearch = w }

// SetTTS enables /语音 and the voice keywords (nil = disabled).
func (h *Handler) SetTTS(t *agent.TTS) { h.tts = t }

// SetWorkspaceManager enables workspace commands (nil = disabled).
func (h *Handler) SetWorkspaceManager(m *WorkspaceManager) { h.workspace = m }

func (h *Handler) isAllowed(userID string) bool {
	for _, u := range h.allowedUsers {
		if u == userID {
			return true
		}
	}
	return false
}

// SetSaveDir sets the directory for saving images and files.
func (h *Handler) SetSaveDir(dir string) {
	h.saveDir = dir
}

// SetCronStore sets the cron job store for /cron commands.
func (h *Handler) SetCronStore(store *CronStore) {
	h.cronStore = store
}

// SetMemoryStore enables /记住, /记忆 and /忘记.
func (h *Handler) SetMemoryStore(store *MemoryStore) {
	h.memoryStore = store
}

// cleanSeenMsgs removes entries older than 5 minutes from the dedup cache.
func (h *Handler) cleanSeenMsgs() {
	cutoff := time.Now().Add(-5 * time.Minute)
	h.seenMsgs.Range(func(key, value any) bool {
		if t, ok := value.(time.Time); ok && t.Before(cutoff) {
			h.seenMsgs.Delete(key)
		}
		return true
	})
}

// SetCustomAliases sets custom alias mappings from config.
func (h *Handler) SetCustomAliases(aliases map[string]string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.customAliases = aliases
}

// SetAgentMetas sets the list of all configured agents (for /status).
func (h *Handler) SetAgentMetas(metas []AgentMeta) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.agentMetas = metas
}

// SetAgentWorkDirs sets the configured working directory for each agent.
func (h *Handler) SetAgentWorkDirs(workDirs map[string]string) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.agentWorkDirs = make(map[string]string, len(workDirs))
	for name, dir := range workDirs {
		h.agentWorkDirs[name] = dir
	}
}

// SetDefaultAgent registers an already-started agent. It only takes over as
// the default when no default is set yet, or when the same agent name is still
// the current default. This avoids background startup races from clobbering a
// newer user-selected default.
func (h *Handler) SetDefaultAgent(name string, ag agent.Agent) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.agents[name] = ag
	if h.defaultName == "" || h.defaultName == name {
		h.defaultName = name
		log.Printf("[handler] default agent ready: %s (%s)", name, ag.Info())
		return
	}

	log.Printf("[handler] agent ready without switching default: started=%s current_default=%s (%s)", name, h.defaultName, ag.Info())
}

// getAgent returns a running agent by name, or starts it on demand via factory.
func (h *Handler) getAgent(ctx context.Context, name string) (agent.Agent, error) {
	// Fast path: already running
	h.mu.RLock()
	ag, ok := h.agents[name]
	h.mu.RUnlock()
	if ok {
		return ag, nil
	}

	// Slow path: create on demand
	if h.factory == nil {
		return nil, fmt.Errorf("agent %q not found and no factory configured", name)
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	// Double-check after acquiring write lock
	if ag, ok := h.agents[name]; ok {
		return ag, nil
	}

	log.Printf("[handler] starting agent %q on demand...", name)
	ag = h.factory(ctx, name)
	if ag == nil {
		return nil, fmt.Errorf("agent %q not available", name)
	}

	h.agents[name] = ag
	log.Printf("[handler] agent started on demand: %s (%s)", name, ag.Info())
	return ag, nil
}

// getDefaultAgent returns the default agent (may be nil if not ready yet).
func (h *Handler) getDefaultAgent() agent.Agent {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if h.defaultName == "" {
		return nil
	}
	return h.agents[h.defaultName]
}

// GetDefaultAgent returns the default agent (exported for cron/heartbeat).
func (h *Handler) GetDefaultAgent() agent.Agent {
	return h.getDefaultAgent()
}

// GetAgent returns a running agent by name (exported for cron).
func (h *Handler) GetAgent(ctx context.Context, name string) (agent.Agent, error) {
	return h.getAgent(ctx, name)
}

// isKnownAgent checks if a name corresponds to a configured agent.
func (h *Handler) isKnownAgent(name string) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	// Check running agents
	if _, ok := h.agents[name]; ok {
		return true
	}
	// Check configured agents (metas)
	for _, meta := range h.agentMetas {
		if meta.Name == name {
			return true
		}
	}
	return false
}

// agentAliases maps short aliases to agent config names.
var agentAliases = map[string]string{
	"cc":  "claude",
	"cx":  "codex",
	"hm":  "hermes",
	"oc":  "openclaw",
	"cs":  "cursor",
	"km":  "kimi",
	"gm":  "gemini",
	"ocd": "opencode",
	"pi":  "pi",
	"cp":  "copilot",
	"dr":  "droid",
	"if":  "iflow",
	"kr":  "kiro",
	"qw":  "qwen",
}

// resolveAlias returns the full agent name for an alias, or the original name if no alias matches.
// Checks custom aliases (from config) first, then built-in aliases.
func (h *Handler) resolveAlias(name string) string {
	h.mu.RLock()
	custom := h.customAliases
	h.mu.RUnlock()
	if custom != nil {
		if full, ok := custom[name]; ok {
			return full
		}
	}
	if full, ok := agentAliases[name]; ok {
		return full
	}
	return name
}

// parseCommand checks if text starts with "/" or "@" followed by agent name(s).
// Supports multiple agents: "@cc @cx hello" returns (["claude","codex"], "hello").
// Returns (agentNames, actualMessage). Aliases are resolved automatically.
// If no command prefix, returns (nil, originalText).
func (h *Handler) parseCommand(text string) ([]string, string) {
	if !strings.HasPrefix(text, "/") && !strings.HasPrefix(text, "@") {
		return nil, text
	}

	// Parse consecutive @name or /name tokens from the start
	var names []string
	rest := text
	for {
		rest = strings.TrimSpace(rest)
		if !strings.HasPrefix(rest, "/") && !strings.HasPrefix(rest, "@") {
			break
		}

		// Strip prefix
		after := rest[1:]
		idx := strings.IndexAny(after, " /@")
		var token string
		if idx < 0 {
			// Rest is just the name, no message
			token = after
			rest = ""
		} else if after[idx] == '/' || after[idx] == '@' {
			// Next token is another @name or /name
			token = after[:idx]
			rest = after[idx:]
		} else {
			// Space — name ends here
			token = after[:idx]
			rest = strings.TrimSpace(after[idx+1:])
		}

		if token != "" {
			names = append(names, h.resolveAlias(token))
		}

		if rest == "" {
			break
		}
	}

	// Deduplicate names preserving order
	seen := make(map[string]bool)
	unique := names[:0]
	for _, n := range names {
		if !seen[n] {
			seen[n] = true
			unique = append(unique, n)
		}
	}

	return unique, rest
}

// HandleMessage processes a single incoming message.
func (h *Handler) HandleMessage(ctx context.Context, client *ilink.Client, msg ilink.WeixinMessage) {
	// Only process user messages that are finished
	if msg.MessageType != ilink.MessageTypeUser {
		return
	}
	if msg.MessageState != ilink.MessageStateFinish {
		return
	}

	// Serve only the owner list when configured; stay silent otherwise so an
	// unlisted sender learns nothing about the bot.
	if len(h.allowedUsers) > 0 && !h.isAllowed(msg.FromUserID) {
		log.Printf("[handler] ignored message from unlisted user %s", msg.FromUserID)
		return
	}
	// Deduplicate by message_id to avoid processing the same message multiple times
	// (voice messages may trigger multiple finish-state updates)
	if msg.MessageID != 0 {
		if _, loaded := h.seenMsgs.LoadOrStore(msg.MessageID, time.Now()); loaded {
			return
		}
		// Clean up old entries periodically (fire-and-forget)
		go h.cleanSeenMsgs()
	}

	// Store context token for this user
	if msg.ContextToken != "" {
		h.contextTokens.Store(msg.FromUserID, msg.ContextToken)
	}
	if err := ilink.SaveContextToken(client.BotID(), msg.FromUserID, msg.ContextToken); err != nil {
		log.Printf("[handler] failed to save context token for %s: %v", msg.FromUserID, err)
	}
	// Deliver any notifications queued while the reply context was expired.
	go FlushPendingNotifications(ctx, client, msg.FromUserID, msg.ContextToken)

	// Extract text from item list (text message or voice transcription)
	text := extractText(msg)
	if text == "" {
		if voiceText := extractVoiceText(msg); voiceText != "" {
			text = voiceText
			log.Printf("[handler] voice transcription from %s: %q", msg.FromUserID, truncate(text, 80))
		}
	}
	if strings.TrimSpace(text) != "" {
		rememberQuoteMessage(client.BotID(), msg.FromUserID, "inbound", text, quoteIDsForMessage(msg)...)
	}
	images := extractImages(msg)
	if len(images) > 0 {
		h.handleImageMessage(ctx, client, msg, text, images)
		return
	}
	if video := extractVideo(msg); video != nil {
		h.handleVideoMessage(ctx, client, msg, text, video)
		return
	}
	if file := extractFile(msg); file != nil {
		// Save and index the body even when the same message carries a caption;
		// the caption keeps flowing so it can ask about the file right away.
		h.handleIncomingFile(ctx, client, msg, file, strings.TrimSpace(text) == "")
		if strings.TrimSpace(text) == "" {
			return
		}
	}
	if text == "" {
		if findQuoteItem(msg) != nil {
			text = "请看看我引用的这条消息。"
		} else {
			log.Printf("[handler] received non-text message from %s, skipping", msg.FromUserID)
			return
		}
	}

	log.Printf("[handler] received from %s: %q", msg.FromUserID, truncate(text, 80))

	// Generate a clientID for this reply (used to correlate typing → finish)
	clientID := NewClientID()

	trimmed := strings.TrimSpace(text)

	// The monitor normally delivers /stop out of band; this is the fallback for
	// callers that invoke the handler directly.
	if trimmed == "/stop" {
		h.handleStopCommand(ctx, client, msg, clientID)
		return
	}

	// Keep bridge-only commands like /cwd in WeClaw, but let Hermes own its
	// native slash commands when Hermes is the active default agent.
	if !h.shouldBypassBridgeCommand(trimmed) {
		if trimmed == "/info" {
			reply := h.buildStatus()
			if err := SendTextReply(ctx, client, msg.FromUserID, reply, msg.ContextToken, clientID); err != nil {
				log.Printf("[handler] failed to send reply to %s: %v", msg.FromUserID, err)
			}
			return
		} else if trimmed == "/help" {
			reply := buildHelpText()
			if err := SendTextReply(ctx, client, msg.FromUserID, reply, msg.ContextToken, clientID); err != nil {
				log.Printf("[handler] failed to send reply to %s: %v", msg.FromUserID, err)
			}
			return
		} else if trimmed == "/new" || trimmed == "/clear" {
			reply := h.resetDefaultSession(ctx, msg.FromUserID)
			if err := SendTextReply(ctx, client, msg.FromUserID, reply, msg.ContextToken, clientID); err != nil {
				log.Printf("[handler] failed to send reply to %s: %v", msg.FromUserID, err)
			}
			return
		}
	}

	if h.handleMemoryCommand(ctx, client, msg, trimmed, clientID) {
		return
	}
	if h.handleReminderRequest(ctx, client, msg, trimmed, clientID) {
		return
	}
	if query, ok := detectSearchRequest(trimmed); ok {
		h.handleSearchRequest(ctx, client, msg, query, clientID)
		return
	}
	if prompt, ok := detectImageRequest(textWithoutSize(trimmed)); ok {
		size, _ := extractImageSize(trimmed)
		h.handleImageRequest(ctx, client, msg, prompt, size, clientID)
		return
	}
	if prompt, ok := detectVideoRequest(textWithoutSize(trimmed)); ok {
		size, _ := extractVideoSize(trimmed)
		h.handleVideoRequest(ctx, client, msg, prompt, size, clientID)
		return
	}
	if h.handleVoiceCommand(ctx, client, msg, trimmed, clientID) {
		return
	}
	if _, ok := parseSearchEngineArgs(trimmed); ok {
		reply := h.handleSearchEngineCommand(trimmed)
		if err := SendTextReply(ctx, client, msg.FromUserID, reply, msg.ContextToken, clientID); err != nil {
			log.Printf("[handler] failed to send /se reply: %v", err)
		}
		return
	}
	if trimmed == "/mod" || strings.HasPrefix(trimmed, "/mod ") || strings.HasPrefix(trimmed, "/mod-") {
		reply := h.handleModCommand(ctx, trimmed)
		if err := SendTextReply(ctx, client, msg.FromUserID, reply, msg.ContextToken, clientID); err != nil {
			log.Printf("[handler] failed to send /mod reply: %v", err)
		}
		return
	}
	if strings.HasPrefix(trimmed, "/cwd") {
		reply := h.handleCwd(ctx, msg.FromUserID, trimmed)
		if err := SendTextReply(ctx, client, msg.FromUserID, reply, msg.ContextToken, clientID); err != nil {
			log.Printf("[handler] failed to send reply to %s: %v", msg.FromUserID, err)
		}
		return
	} else if strings.HasPrefix(trimmed, "/cron") {
		if h.cronStore == nil {
			_ = SendTextReply(ctx, client, msg.FromUserID, "Cron is not configured.", msg.ContextToken, clientID)
			return
		}
		reply := HandleCronCommand(h.cronStore, trimmed, msg.FromUserID)
		if err := SendTextReply(ctx, client, msg.FromUserID, reply, msg.ContextToken, clientID); err != nil {
			log.Printf("[handler] failed to send cron reply to %s: %v", msg.FromUserID, err)
		}
		return
	}

	if h.handleWorkspaceCommand(ctx, client, msg, trimmed, clientID) {
		return
	}
	if h.handleExportCommand(ctx, client, msg, trimmed, clientID) {
		return
	}

	if h.handleLinkText(ctx, client, msg, trimmed, clientID) {
		return
	}

	// Route: "/agentname message" or "@agent1 @agent2 message" -> specific agent(s)
	agentNames, message := h.parseCommand(text)

	// No command prefix -> send to default agent
	if len(agentNames) == 0 {
		h.sendToDefaultAgent(ctx, client, msg, text, clientID)
		return
	}

	// No message -> switch default agent (only first name)
	if message == "" {
		if len(agentNames) == 1 && h.isKnownAgent(agentNames[0]) {
			reply := h.switchDefault(ctx, agentNames[0])
			if err := SendTextReply(ctx, client, msg.FromUserID, reply, msg.ContextToken, clientID); err != nil {
				log.Printf("[handler] failed to send reply to %s: %v", msg.FromUserID, err)
			}
		} else if len(agentNames) == 1 && !h.isKnownAgent(agentNames[0]) {
			// Unknown agent -> forward to default
			h.sendToDefaultAgent(ctx, client, msg, text, clientID)
		} else {
			reply := "Usage: specify one agent to switch, or add a message to broadcast"
			if err := SendTextReply(ctx, client, msg.FromUserID, reply, msg.ContextToken, clientID); err != nil {
				log.Printf("[handler] failed to send reply to %s: %v", msg.FromUserID, err)
			}
		}
		return
	}

	// Filter to known agents; if single unknown agent -> forward to default
	var knownNames []string
	for _, name := range agentNames {
		if h.isKnownAgent(name) {
			knownNames = append(knownNames, name)
		}
	}
	if len(knownNames) == 0 {
		// No known agents -> forward entire text to default agent
		h.sendToDefaultAgent(ctx, client, msg, text, clientID)
		return
	}

	// Send typing indicator
	go func() {
		if typingErr := SendTypingState(ctx, client, msg.FromUserID, msg.ContextToken); typingErr != nil {
			log.Printf("[handler] failed to send typing state: %v", typingErr)
		}
	}()

	if len(knownNames) == 1 {
		// Single agent
		h.sendToNamedAgent(ctx, client, msg, knownNames[0], message, clientID)
	} else {
		// Multi-agent broadcast: parallel dispatch, send replies as they arrive
		h.broadcastToAgents(ctx, client, msg, knownNames, message)
	}
}

// HandleMessages processes a merged group from one sender. Plain text and
// images are folded into one agent turn; commands, quotes, files and videos
// keep their original per-message semantics.
func (h *Handler) HandleMessages(ctx context.Context, client *ilink.Client, msgs []ilink.WeixinMessage) {
	filtered := make([]ilink.WeixinMessage, 0, len(msgs))
	for _, msg := range msgs {
		if msg.MessageType != ilink.MessageTypeUser || msg.MessageState != ilink.MessageStateFinish {
			continue
		}
		filtered = append(filtered, msg)
	}
	if len(filtered) == 0 {
		return
	}
	if len(filtered) == 1 {
		h.HandleMessage(ctx, client, filtered[0])
		return
	}

	firstUser := filtered[0].FromUserID
	for _, msg := range filtered[1:] {
		if msg.FromUserID != firstUser {
			for _, item := range filtered {
				h.HandleMessage(ctx, client, item)
			}
			return
		}
	}
	if h.batchNeedsSequential(filtered) {
		for _, item := range filtered {
			if ctx.Err() != nil {
				return
			}
			h.HandleMessage(ctx, client, item)
		}
		return
	}

	h.handleMergedMessages(ctx, client, filtered)
}

func (h *Handler) batchNeedsSequential(msgs []ilink.WeixinMessage) bool {
	for _, msg := range msgs {
		if findQuoteItem(msg) != nil || extractVideo(msg) != nil || extractFile(msg) != nil {
			return true
		}
		if text := strings.TrimSpace(extractText(msg)); strings.HasPrefix(text, "/") || strings.HasPrefix(text, "@") {
			return true
		}
		for _, item := range msg.ItemList {
			if item.Type == ilink.ItemTypeVoice && (item.VoiceItem == nil || strings.TrimSpace(item.VoiceItem.Text) == "") {
				return true
			}
		}
	}
	return false
}

func (h *Handler) handleMergedMessages(ctx context.Context, client *ilink.Client, msgs []ilink.WeixinMessage) {
	if len(h.allowedUsers) > 0 && !h.isAllowed(msgs[0].FromUserID) {
		log.Printf("[handler] ignored merged messages from unlisted user %s", msgs[0].FromUserID)
		return
	}

	clientID := NewClientID()
	anchor := msgs[len(msgs)-1]
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].ContextToken != "" {
			anchor.ContextToken = msgs[i].ContextToken
			break
		}
	}
	if anchor.ContextToken != "" {
		h.contextTokens.Store(anchor.FromUserID, anchor.ContextToken)
		if err := ilink.SaveContextToken(client.BotID(), anchor.FromUserID, anchor.ContextToken); err != nil {
			log.Printf("[handler] failed to save context token for %s: %v", anchor.FromUserID, err)
		}
	}
	go FlushPendingNotifications(ctx, client, anchor.FromUserID, anchor.ContextToken)

	var textParts []string
	var attachments []agent.Attachment
	for _, msg := range msgs {
		text := extractText(msg)
		if text == "" {
			text = extractVoiceText(msg)
		}
		if strings.TrimSpace(text) != "" {
			textParts = append(textParts, strings.TrimSpace(text))
			rememberQuoteMessage(client.BotID(), msg.FromUserID, "inbound", text, quoteIDsForMessage(msg)...)
		}
		for _, img := range extractImages(msg) {
			attachment, err := h.saveInboundImage(ctx, msg, img, h.mediaSaveDir())
			if err != nil {
				log.Printf("[handler] failed to save merged image from %s: %v", msg.FromUserID, err)
				h.sendFailureNotice(ctx, client, anchor, fmt.Sprintf("Failed to save image: %v", err), clientID)
				return
			}
			attachments = append(attachments, attachment)
			rememberInboundMedia(client.BotID(), msg, attachment.Path, attachment.MIMEType, attachment.Name)
		}
	}

	mergedText := strings.Join(textParts, "\n\n")
	if mergedText == "" {
		if len(attachments) > 1 {
			mergedText = "Please analyze the attached images."
		} else {
			mergedText = "Please analyze the attached image."
		}
	}
	log.Printf("[handler] merged %d messages from %s: %q (%d image(s))", len(msgs), anchor.FromUserID, truncate(mergedText, 120), len(attachments))

	go func() {
		if err := SendTypingState(ctx, client, anchor.FromUserID, anchor.ContextToken); err != nil {
			log.Printf("[handler] failed to send typing state: %v", err)
		}
	}()

	h.mu.RLock()
	defaultName := h.defaultName
	h.mu.RUnlock()

	var reply string
	if ag := h.getDefaultAgent(); ag != nil {
		var err error
		reply, err = h.chatWithAgentAttachments(ctx, ag, client, anchor, mergedText, attachments)
		if err != nil {
			if canceledReply(err) {
				log.Printf("[handler] merged turn canceled for %s", anchor.FromUserID)
				return
			}
			reply = fmt.Sprintf("Error: %v", err)
		}
	} else if len(attachments) > 0 {
		reply = fmt.Sprintf("Saved image: %s", attachments[0].Path)
	} else {
		reply = "[echo] " + mergedText
	}

	h.sendReplyWithMedia(ctx, client, anchor, defaultName, reply, clientID)
}

// sendToDefaultAgent sends the message to the default agent and replies.
func (h *Handler) sendToDefaultAgent(ctx context.Context, client *ilink.Client, msg ilink.WeixinMessage, text, clientID string) {
	go func() {
		if typingErr := SendTypingState(ctx, client, msg.FromUserID, msg.ContextToken); typingErr != nil {
			log.Printf("[handler] failed to send typing state: %v", typingErr)
		}
	}()

	h.mu.RLock()
	defaultName := h.defaultName
	h.mu.RUnlock()

	ag := h.getDefaultAgent()
	var reply string
	if ag != nil {
		var err error
		reply, err = h.chatWithAgent(ctx, ag, client, msg, text)
		if err != nil {
			if canceledReply(err) {
				log.Printf("[handler] turn canceled for %s", msg.FromUserID)
				return
			}
			reply = fmt.Sprintf("Error: %v", err)
		}
	} else {
		log.Printf("[handler] agent not ready, using echo mode for %s", msg.FromUserID)
		reply = "[echo] " + text
	}

	h.sendReplyWithMedia(ctx, client, msg, defaultName, reply, clientID)
}

// sendToNamedAgent sends the message to a specific agent and replies.
func (h *Handler) sendToNamedAgent(ctx context.Context, client *ilink.Client, msg ilink.WeixinMessage, name, message, clientID string) {
	ag, agErr := h.getAgent(ctx, name)
	if agErr != nil {
		log.Printf("[handler] agent %q not available: %v", name, agErr)
		reply := fmt.Sprintf("Agent %q is not available: %v", name, agErr)
		SendTextReply(ctx, client, msg.FromUserID, reply, msg.ContextToken, clientID)
		return
	}

	reply, err := h.chatWithAgent(ctx, ag, client, msg, message)
	if err != nil {
		if canceledReply(err) {
			log.Printf("[handler] turn canceled for %s (agent %s)", msg.FromUserID, name)
			return
		}
		reply = fmt.Sprintf("Error: %v", err)
	}
	h.sendReplyWithMedia(ctx, client, msg, name, reply, clientID)
}

// broadcastToAgents sends the message to multiple agents in parallel.
// Each reply is sent as a separate message with the agent name prefix.
func (h *Handler) broadcastToAgents(ctx context.Context, client *ilink.Client, msg ilink.WeixinMessage, names []string, message string) {
	type result struct {
		name     string
		reply    string
		canceled bool
	}

	ch := make(chan result, len(names))

	for _, name := range names {
		go func(n string) {
			ag, err := h.getAgent(ctx, n)
			if err != nil {
				ch <- result{name: n, reply: fmt.Sprintf("Error: %v", err)}
				return
			}
			reply, err := h.chatWithAgent(ctx, ag, client, msg, message)
			if err != nil {
				if canceledReply(err) {
					ch <- result{name: n, canceled: true}
					return
				}
				ch <- result{name: n, reply: fmt.Sprintf("Error: %v", err)}
				return
			}
			ch <- result{name: n, reply: reply}
		}(name)
	}

	// Send replies as they arrive
	for range names {
		r := <-ch
		if r.canceled {
			continue
		}
		reply := fmt.Sprintf("[%s] %s", r.name, r.reply)
		clientID := NewClientID()
		h.sendReplyWithMedia(ctx, client, msg, r.name, reply, clientID)
	}
}

// sendReplyWithMedia sends a text reply and any extracted image URLs.
func (h *Handler) sendReplyWithMedia(ctx context.Context, client *ilink.Client, msg ilink.WeixinMessage, agentName, reply, clientID string) {
	imageURLs := ExtractImageURLs(reply)
	attachmentPaths := extractLocalAttachmentPaths(reply)
	allowedRoots := h.allowedAttachmentRoots(agentName, msg.FromUserID)

	var sentPaths []string
	var failedPaths []string
	for _, attachmentPath := range attachmentPaths {
		if !isAllowedAttachmentPath(attachmentPath, allowedRoots) {
			log.Printf("[handler] rejected attachment outside allowed roots for agent %q: %s", agentName, attachmentPath)
			failedPaths = append(failedPaths, attachmentPath)
			continue
		}
		if err := SendMediaFromPath(ctx, client, msg.FromUserID, attachmentPath, msg.ContextToken); err != nil {
			log.Printf("[handler] failed to send attachment to %s: %v", msg.FromUserID, err)
			failedPaths = append(failedPaths, attachmentPath)
			continue
		}
		sentPaths = append(sentPaths, attachmentPath)
	}

	if !errorNoticesEnabled.Load() {
		failedPaths = nil
	}
	reply = rewriteReplyWithAttachmentResults(reply, sentPaths, failedPaths)

	if strings.TrimSpace(reply) != "" {
		if err := SendTextReply(ctx, client, msg.FromUserID, reply, msg.ContextToken, clientID); err != nil {
			log.Printf("[handler] failed to send reply to %s: %v", msg.FromUserID, err)
		}
	}

	for _, imgURL := range imageURLs {
		if err := SendMediaFromURL(ctx, client, msg.FromUserID, imgURL, msg.ContextToken); err != nil {
			log.Printf("[handler] failed to send image to %s: %v", msg.FromUserID, err)
		}
	}
}

func (h *Handler) allowedAttachmentRoots(agentName, userID string) []string {
	roots := []string{defaultAttachmentWorkspace()}

	h.mu.RLock()
	agentDir := h.agentWorkDirs[agentName]
	h.mu.RUnlock()

	if agentDir != "" {
		roots = append(roots, agentDir)
	}
	if h.workspace != nil {
		if wsRoot := h.workspace.CurrentRoot(userID); wsRoot != "" {
			roots = append(roots, wsRoot)
		}
	}

	return roots
}

// chatWithAgent sends a message to an agent and returns the reply, with logging.
func (h *Handler) chatWithAgent(ctx context.Context, ag agent.Agent, client *ilink.Client, msg ilink.WeixinMessage, message string) (string, error) {
	return h.chatWithAgentAttachments(ctx, ag, client, msg, message, nil)
}

func (h *Handler) chatWithAgentAttachments(ctx context.Context, ag agent.Agent, client *ilink.Client, msg ilink.WeixinMessage, message string, attachments []agent.Attachment) (string, error) {
	info := ag.Info()
	userID := msg.FromUserID
	taskCtx, finish := h.beginTask(ctx, userID, "AI 回复", func(stopCtx context.Context) {
		if canceller, ok := ag.(agent.TurnCanceler); ok {
			if cancelErr := canceller.CancelTurn(stopCtx, userID); cancelErr != nil {
				log.Printf("[stop] cancel agent turn for %s: %v", userID, cancelErr)
			}
		}
	})
	defer finish()

	if len(attachments) == 0 && !strings.HasPrefix(strings.TrimSpace(message), "/") {
		message = h.attachPendingIncomingFile(userID, info.Type, message)
	}
	log.Printf("[handler] dispatching to agent (%s) for %s", info, userID)

	start := time.Now()
	if len(attachments) == 0 {
		if reply, handled, err := h.handleHermesNativeCommand(taskCtx, ag, userID, message); handled {
			elapsed := time.Since(start)
			if taskCtx.Err() != nil {
				return "", context.Canceled
			}
			if err != nil {
				log.Printf("[handler] hermes native command error (%s, elapsed=%s): %v", info, elapsed, err)
				return "", err
			}
			log.Printf("[handler] hermes native command reply (%s, elapsed=%s): %q", info, elapsed, truncate(reply, 100))
			return reply, nil
		}
	}

	if strings.TrimSpace(message) != "" && !strings.HasPrefix(strings.TrimSpace(message), "/") {
		if mem := h.memoryPromptFor(userID); mem != "" {
			message = mem + "\n\n" + message
			log.Printf("[memory] injected saved notes for %s", userID)
		}
		if quotePrompt, quoteAttachments := h.quoteContextForMessage(taskCtx, client, msg); quotePrompt != "" {
			message = quotePrompt + "\n\n" + message
			attachments = append(attachments, quoteAttachments...)
			log.Printf("[quote] injected quoted context for %s (%d chars, %d attachment(s))", userID, len(quotePrompt), len(quoteAttachments))
		}
	}

	var reply string
	var err error
	if attachmentProgressAgent, ok := ag.(agent.AttachmentProgressChatAgent); ok {
		reply, err = attachmentProgressAgent.ChatWithAttachmentsAndProgress(taskCtx, userID, message, attachments, h.toolProgressForwarder(ctx, client, msg))
	} else if attachmentAgent, ok := ag.(agent.AttachmentChatAgent); ok {
		reply, err = attachmentAgent.ChatWithAttachments(taskCtx, userID, message, attachments)
	} else if progressAgent, ok := ag.(agent.ProgressChatAgent); ok {
		message = agent.MessageWithAttachmentFallback(message, attachments)
		reply, err = progressAgent.ChatWithProgress(taskCtx, userID, message, h.toolProgressForwarder(ctx, client, msg))
	} else {
		message = agent.MessageWithAttachmentFallback(message, attachments)
		reply, err = ag.Chat(taskCtx, userID, message)
	}
	elapsed := time.Since(start)

	if taskCtx.Err() != nil {
		log.Printf("[handler] agent turn canceled (%s, elapsed=%s)", info, elapsed)
		return "", context.Canceled
	}
	if err != nil {
		log.Printf("[handler] agent error (%s, elapsed=%s): %v", info, elapsed, err)
		return "", err
	}

	log.Printf("[handler] agent replied (%s, elapsed=%s): %q", info, elapsed, truncate(reply, 100))
	return reply, nil
}

func (h *Handler) toolProgressForwarder(ctx context.Context, client *ilink.Client, msg ilink.WeixinMessage) func(agent.ToolProgressEvent) {
	return func(evt agent.ToolProgressEvent) {
		toolName := strings.TrimSpace(evt.ToolName)
		if toolName == "" {
			toolName = "tool"
		}

		clientID := NewClientID()
		var err error
		switch evt.Phase {
		case "start":
			err = SendToolCallStart(ctx, client, msg.FromUserID, toolName, evt.ToolCallID, msg.ContextToken, clientID)
		case "end":
			err = SendToolCallResult(ctx, client, msg.FromUserID, toolName, evt.ToolCallID, evt.Status, msg.ContextToken, clientID)
		}
		if err != nil {
			log.Printf("[handler] failed to send tool progress item: %v", err)
		}
	}
}

func (h *Handler) handleHermesNativeCommand(ctx context.Context, ag agent.Agent, userID, message string) (string, bool, error) {
	if !isHermesAgent(ag) {
		return "", false, nil
	}

	trimmed := strings.TrimSpace(message)
	if !strings.HasPrefix(trimmed, "/") {
		return "", false, nil
	}

	switch trimmed {
	case "/help":
		return buildHermesHelpText(), true, nil
	case "/skills":
		return buildHermesSkillsHelpText(), true, nil
	case "/new", "/clear":
		sessionID, err := ag.ResetSession(ctx, userID)
		if err != nil {
			return "", true, fmt.Errorf("failed to reset Hermes session: %w", err)
		}
		if sessionID != "" {
			return fmt.Sprintf("Started a new Hermes session\n%s", sessionID), true, nil
		}
		return "Started a new Hermes session", true, nil
	case "/info":
		reply, err := runHermesCLI(ctx, ag.Info().Command, "status")
		return reply, true, err
	case "/version":
		reply, err := runHermesCLI(ctx, ag.Info().Command, "version")
		return reply, true, err
	}

	if strings.HasPrefix(trimmed, "/skills ") {
		args := append([]string{"skills"}, strings.Fields(strings.TrimSpace(strings.TrimPrefix(trimmed, "/skills")))...)
		reply, err := runHermesCLI(ctx, ag.Info().Command, args...)
		return reply, true, err
	}

	return "", false, nil
}

func isHermesAgent(ag agent.Agent) bool {
	info := ag.Info()
	command := strings.ToLower(filepath.Base(info.Command))
	name := strings.ToLower(strings.TrimSpace(info.Name))
	return strings.Contains(command, "hermes") || strings.Contains(name, "hermes")
}

func runHermesCLI(ctx context.Context, command string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, command, args...)
	cmd.Env = append(os.Environ(), "NO_COLOR=1", "TERM=dumb", "CLICOLOR=0")
	output, err := cmd.CombinedOutput()
	text := strings.TrimSpace(string(output))
	if err != nil {
		if text != "" {
			return text, nil
		}
		return "", err
	}
	if text == "" {
		return "No output.", nil
	}
	return text, nil
}

// switchDefault switches the default agent. Starts it on demand if needed.
// The change is persisted to config file.
func (h *Handler) switchDefault(ctx context.Context, name string) string {
	ag, err := h.getAgent(ctx, name)
	if err != nil {
		log.Printf("[handler] failed to switch default to %q: %v", name, err)
		return fmt.Sprintf("Failed to switch to %q: %v", name, err)
	}

	h.mu.Lock()
	old := h.defaultName
	h.defaultName = name
	h.agents[name] = ag
	h.mu.Unlock()

	// Persist to config file
	if h.saveDefault != nil {
		if err := h.saveDefault(name); err != nil {
			log.Printf("[handler] failed to save default agent to config: %v", err)
		} else {
			log.Printf("[handler] saved default agent %q to config", name)
		}
	}

	info := ag.Info()
	log.Printf("[handler] switched default agent: %s -> %s (%s)", old, name, info)
	return fmt.Sprintf("switch to %s", name)
}

// resetDefaultSession resets the session for the given userID on the default agent.
func (h *Handler) resetDefaultSession(ctx context.Context, userID string) string {
	ag := h.getDefaultAgent()
	if ag == nil {
		return "No agent running."
	}
	h.pendingFiles.Delete(userID)
	name := ag.Info().Name
	sessionID, err := ag.ResetSession(ctx, userID)
	if err != nil {
		log.Printf("[handler] reset session failed for %s: %v", userID, err)
		return fmt.Sprintf("Failed to reset session: %v", err)
	}
	if sessionID != "" {
		return fmt.Sprintf("已创建新的%s会话\n%s", name, sessionID)
	}
	return fmt.Sprintf("已创建新的%s会话", name)
}

// handleCwd handles the /cwd command. It updates the working directory for all running agents.
func (h *Handler) handleCwd(ctx context.Context, userID string, trimmed string) string {
	arg := strings.TrimSpace(strings.TrimPrefix(trimmed, "/cwd"))
	if arg == "" {
		// No path provided — show current cwd of default agent
		ag := h.getDefaultAgent()
		if ag == nil {
			return "No agent running."
		}
		info := ag.Info()
		return fmt.Sprintf("cwd: (check agent config)\nagent: %s", info.Name)
	}

	// Expand ~ to home directory
	if arg == "~" {
		home, err := os.UserHomeDir()
		if err == nil {
			arg = home
		}
	} else if strings.HasPrefix(arg, "~/") {
		home, err := os.UserHomeDir()
		if err == nil {
			arg = filepath.Join(home, arg[2:])
		}
	}

	// Resolve to absolute path
	absPath, err := filepath.Abs(arg)
	if err != nil {
		return fmt.Sprintf("Invalid path: %v", err)
	}

	// Verify directory exists
	info, err := os.Stat(absPath)
	if err != nil {
		return fmt.Sprintf("Path not found: %s", absPath)
	}
	if !info.IsDir() {
		return fmt.Sprintf("Not a directory: %s", absPath)
	}

	// Update cwd on all running agents
	h.mu.RLock()
	agents := make(map[string]agent.Agent, len(h.agents))
	for name, ag := range h.agents {
		agents[name] = ag
	}
	h.mu.RUnlock()

	for name, ag := range agents {
		ag.SetCwd(absPath)
		log.Printf("[handler] updated cwd for agent %s: %s", name, absPath)
		if userID == "" {
			continue
		}
		resetCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		if _, err := ag.ResetSession(resetCtx, userID); err != nil {
			log.Printf("[handler] failed to reset session after cwd update for agent %s: %v", name, err)
		}
		cancel()
	}

	h.mu.Lock()
	if h.agentWorkDirs == nil {
		h.agentWorkDirs = make(map[string]string, len(agents))
	}
	for name := range agents {
		h.agentWorkDirs[name] = absPath
	}
	h.mu.Unlock()

	return fmt.Sprintf("cwd: %s", absPath)
}

// buildStatus returns a short status string showing the current default agent.
func (h *Handler) buildStatus() string {
	h.mu.RLock()
	defer h.mu.RUnlock()

	if h.defaultName == "" {
		return "agent: none (echo mode)"
	}

	ag, ok := h.agents[h.defaultName]
	if !ok {
		return fmt.Sprintf("agent: %s (not started)", h.defaultName)
	}

	info := ag.Info()
	return fmt.Sprintf("agent: %s\n\ntype: %s\n\nmodel: %s", h.defaultName, info.Type, info.Model)
}

// WeChat folds single newlines into one paragraph; only blank lines render as
// line breaks.
func wechatLines(text string) string {
	lines := strings.Split(text, "\n")
	kept := lines[:0]
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n\n")
}

func buildHelpText() string {
	return wechatLines(`WeClaw 命令帮助

【基本】
@agent 或 /agent - 切换默认 AI 助手
@agent 消息 或 /agent 消息 - 把消息发给指定 AI 助手
@a @b 消息 - 同时发给多个 AI 助手
/new 或 /clear - 开始新会话
/stop - 中止正在运行的长任务
/cwd /路径 - 切换工作目录

【代码工作区】
/ws [list|new N|use N] - 列出、新建或切换代码工作区
/ws import <公开 GitHub 链接> - 导入公开仓库；先发 zip 再发 /ws import 可导入压缩包
/code <任务> - 在当前工作区写代码或改代码
/files [目录] - 列出工作区文件
/cat <路径> - 查看工作区文件内容
/write <路径> <内容> - 写入工作区文件
/run <命令> - 在工作区执行命令（需开启 allow_shell）
/diff - 查看工作区改动
/pack - 打包工作区并回传
/send <路径> - 回传工作区文件

【定时与提醒】
/cron list|add|delete|enable|disable - 管理定时任务
提醒我明天上午9点交报告 - 一次性自然语言提醒
每天下午5点提醒我写日报 - 重复自然语言提醒

【长期记忆】
/记住 <内容> - 保存长期记忆（最多 200 条）
/记忆 [页码] - 查看已保存的记忆（每页 10 条）
/忘记 <编号|关键词> - 删除一条记忆

【阅读与文件】
/读 <链接> 或直接发链接 - 读取网页并总结
直接发文件 - 分析文本/代码、Word、Excel，不支持 PDF
/word [内容] - 导出 Word；不带内容时用刚上传的文件
/excel [内容] - 导出 Excel；单元格用 | 分列
/导出 word|excel <内容> - 中文写法，效果同上

【图片与视频】
/图 <描述> 或 画一只猫 - 生成图片（免费）
/视频 <描述> - 生成视频（免费）

【搜索与模型】
/s <关键词> 或 搜一下… - 联网搜索（默认 DuckDuckGo）
/se [list|N|name] - 查看或切换搜索引擎
/gm - 切到 Gemini，再发 /mod 选免费型号
/mod [list|N|name] - 查看或切换模型（仅 HTTP 助手）
/mod-t - 测试当前可用模型

【语音】
/语音 <文字> 或 用语音说… - 文字转语音，发送可播放的视频气泡（默认晓晓）
/音色 - 查看音色；发 /语音 <音色> <文字> 可指定

【信息】
/info - 查看当前 AI 助手信息
/help - 查看本帮助

助手别名：/cc(claude) /cx(codex) /hm(hermes) /cs(cursor) /km(kimi) /gm(gemini) /oc(openclaw) /ocd(opencode) /pi(pi) /cp(copilot) /dr(droid) /if(iflow) /kr(kiro) /qw(qwen)`)
}

func buildHermesHelpText() string {
	return wechatLines(`Hermes 命令帮助

/help - 查看 Hermes 命令帮助
/skills - 查看技能中心命令
/skills <子命令> - 执行技能中心子命令
/new 或 /clear - 开始新的 Hermes 会话
/info - 查看 Hermes 状态
/version - 查看 Hermes 版本
/model - Hermes 模型命令
/tools - 查看 Hermes 工具列表
/context - 查看 Hermes 上下文摘要
/reset - 重置 Hermes
/compact - 压缩 Hermes 上下文

Hermes 是当前助手时，这些命令由 Hermes 处理。`)
}

func buildHermesSkillsHelpText() string {
	return wechatLines(`Hermes 技能中心命令

browse [--source official] - 浏览全部可用技能（分页）
search <关键词> - 搜索技能
install <标识> - 安装技能（含安全扫描）
inspect <标识> - 预览技能，不安装
list [--source hub|builtin|local] - 查看已安装技能
check [名称] - 检查技能的上游更新
update [名称] - 更新技能到上游版本
audit [名称] - 重新扫描技能安全
uninstall <名称> - 卸载从技能中心安装的技能
publish <路径> --repo <仓库> - 通过 PR 发布技能到 GitHub
snapshot export|import - 导出或导入技能配置
tap list|add|remove - 管理技能来源`)
}

func extractText(msg ilink.WeixinMessage) string {
	for _, item := range msg.ItemList {
		if item.Type == ilink.ItemTypeText && item.TextItem != nil {
			return item.TextItem.Text
		}
	}
	return ""
}

func extractImages(msg ilink.WeixinMessage) []*ilink.ImageItem {
	var images []*ilink.ImageItem
	for _, item := range msg.ItemList {
		if item.Type == ilink.ItemTypeImage && item.ImageItem != nil {
			images = append(images, item.ImageItem)
		}
	}
	return images
}

func extractFile(msg ilink.WeixinMessage) *ilink.FileItem {
	for _, item := range msg.ItemList {
		if item.Type == ilink.ItemTypeFile && item.FileItem != nil {
			return item.FileItem
		}
	}
	return nil
}

func extractVoiceText(msg ilink.WeixinMessage) string {
	for _, item := range msg.ItemList {
		if item.Type == ilink.ItemTypeVoice && item.VoiceItem != nil && item.VoiceItem.Text != "" {
			return item.VoiceItem.Text
		}
	}
	return ""
}

func (h *Handler) handleImageMessage(ctx context.Context, client *ilink.Client, msg ilink.WeixinMessage, text string, images []*ilink.ImageItem) {
	clientID := NewClientID()
	saveDir := h.mediaSaveDir()
	log.Printf("[handler] received %d image(s) from %s, saving to %s", len(images), msg.FromUserID, saveDir)

	var attachments []agent.Attachment
	for _, img := range images {
		attachment, err := h.saveInboundImage(ctx, msg, img, saveDir)
		if err != nil {
			log.Printf("[handler] failed to save inbound image from %s: %v", msg.FromUserID, err)
			h.sendFailureNotice(ctx, client, msg, fmt.Sprintf("Failed to save image: %v", err), clientID)
			return
		}
		attachments = append(attachments, attachment)
		rememberInboundMedia(client.BotID(), msg, attachment.Path, attachment.MIMEType, attachment.Name)
	}

	if strings.TrimSpace(text) == "" {
		text = "Please analyze the attached image."
	}

	go func() {
		if typingErr := SendTypingState(ctx, client, msg.FromUserID, msg.ContextToken); typingErr != nil {
			log.Printf("[handler] failed to send typing state: %v", typingErr)
		}
	}()

	h.mu.RLock()
	defaultName := h.defaultName
	h.mu.RUnlock()

	ag := h.getDefaultAgent()
	var reply string
	if ag != nil {
		var err error
		reply, err = h.chatWithAgentAttachments(ctx, ag, client, msg, text, attachments)
		if err != nil {
			if canceledReply(err) {
				log.Printf("[handler] media turn canceled for %s", msg.FromUserID)
				return
			}
			reply = fmt.Sprintf("Error: %v", err)
		}
	} else {
		reply = fmt.Sprintf("Saved image: %s", attachments[0].Path)
	}

	h.sendReplyWithMedia(ctx, client, msg, defaultName, reply, clientID)
}

func (h *Handler) saveInboundImage(ctx context.Context, msg ilink.WeixinMessage, img *ilink.ImageItem, saveDir string) (agent.Attachment, error) {
	var data []byte
	var err error

	if img.URL != "" {
		data, _, err = downloadFile(ctx, img.URL)
	} else {
		data, err = DownloadMediaInfo(ctx, img.Media, img.AESKey)
	}
	if err != nil {
		return agent.Attachment{}, err
	}

	ext := detectImageExt(data)
	ts := time.Now().Format("20060102-150405")
	fileName := fmt.Sprintf("%s-%s%s", ts, uuid.New().String()[:8], ext)
	filePath := filepath.Join(saveDir, fileName)

	if err := os.MkdirAll(saveDir, 0o755); err != nil {
		return agent.Attachment{}, err
	}
	if err := os.WriteFile(filePath, data, 0o644); err != nil {
		return agent.Attachment{}, err
	}

	sidecarPath := filePath + ".sidecar.md"
	sidecarContent := fmt.Sprintf("---\nid: %s\nfrom_user_id: %s\nmessage_id: %d\n---\n", uuid.New().String(), msg.FromUserID, msg.MessageID)
	if err := os.WriteFile(sidecarPath, []byte(sidecarContent), 0o644); err != nil {
		log.Printf("[handler] failed to write sidecar: %v", err)
	}

	mimeType := mimeTypeFromImageExt(ext)
	log.Printf("[handler] saved inbound image to %s (%d bytes)", filePath, len(data))
	return agent.Attachment{
		Path:     filePath,
		MIMEType: mimeType,
		Name:     fileName,
	}, nil
}

func extractVideo(msg ilink.WeixinMessage) *ilink.VideoItem {
	for _, item := range msg.ItemList {
		if item.Type == ilink.ItemTypeVideo && item.VideoItem != nil {
			return item.VideoItem
		}
	}
	return nil
}

func (h *Handler) handleVideoMessage(ctx context.Context, client *ilink.Client, msg ilink.WeixinMessage, text string, video *ilink.VideoItem) {
	clientID := NewClientID()
	saveDir := h.mediaSaveDir()
	log.Printf("[handler] received video from %s, saving to %s", msg.FromUserID, saveDir)

	if video.Media == nil || (video.Media.EncryptQueryParam == "" && video.Media.FullURL == "") {
		log.Printf("[handler] video has no media info from %s", msg.FromUserID)
		return
	}

	data, err := DownloadMediaInfo(ctx, video.Media, "")
	if err != nil {
		log.Printf("[handler] failed to download video from %s: %v", msg.FromUserID, err)
		h.sendFailureNotice(ctx, client, msg, fmt.Sprintf("Failed to save video: %v", err), clientID)
		return
	}

	fileName := fmt.Sprintf("%s-%s.mp4", time.Now().Format("20060102-150405"), uuid.New().String()[:8])
	filePath := filepath.Join(saveDir, fileName)
	if err := os.MkdirAll(saveDir, 0o755); err != nil {
		log.Printf("[handler] failed to create save dir: %v", err)
		return
	}
	if err := os.WriteFile(filePath, data, 0o644); err != nil {
		log.Printf("[handler] failed to write video: %v", err)
		h.sendFailureNotice(ctx, client, msg, fmt.Sprintf("Failed to save video: %v", err), clientID)
		return
	}
	log.Printf("[handler] saved inbound video to %s (%d bytes)", filePath, len(data))
	rememberInboundMedia(client.BotID(), msg, filePath, "video/mp4", fileName)

	if strings.TrimSpace(text) == "" {
		text = "Please analyze the attached video."
	}

	go func() {
		if typingErr := SendTypingState(ctx, client, msg.FromUserID, msg.ContextToken); typingErr != nil {
			log.Printf("[handler] failed to send typing state: %v", typingErr)
		}
	}()

	h.mu.RLock()
	defaultName := h.defaultName
	h.mu.RUnlock()

	attachments := []agent.Attachment{{Path: filePath, MIMEType: "video/mp4", Name: fileName}}
	var reply string
	if ag := h.getDefaultAgent(); ag != nil {
		reply, err = h.chatWithAgentAttachments(ctx, ag, client, msg, text, attachments)
		if err != nil {
			if canceledReply(err) {
				log.Printf("[handler] media turn canceled for %s", msg.FromUserID)
				return
			}
			reply = fmt.Sprintf("Error: %v", err)
		}
	} else {
		reply = fmt.Sprintf("Saved video: %s", filePath)
	}

	h.sendReplyWithMedia(ctx, client, msg, defaultName, reply, clientID)
}

func (h *Handler) mediaSaveDir() string {
	if h.saveDir != "" {
		return h.saveDir
	}
	return defaultAttachmentWorkspace()
}

// sanitizeIncomingFileName strips directory components and characters that are
// unsafe in file names, so a sender-controlled name cannot escape the save dir.
func sanitizeIncomingFileName(name string) string {
	name = filepath.Base(strings.ReplaceAll(strings.TrimSpace(name), "\\", "/"))
	name = strings.Map(func(r rune) rune {
		switch r {
		case '<', '>', ':', '"', '/', '\\', '|', '?', '*':
			return '-'
		default:
			return r
		}
	}, name)
	name = strings.Trim(name, " .")
	if name == "" || name == ".." {
		return ""
	}
	return name
}

func detectImageExt(data []byte) string {
	if len(data) < 4 {
		return ".bin"
	}
	// PNG: 89 50 4E 47
	if data[0] == 0x89 && data[1] == 0x50 && data[2] == 0x4E && data[3] == 0x47 {
		return ".png"
	}
	// JPEG: FF D8 FF
	if data[0] == 0xFF && data[1] == 0xD8 && data[2] == 0xFF {
		return ".jpg"
	}
	// GIF: 47 49 46
	if data[0] == 0x47 && data[1] == 0x49 && data[2] == 0x46 {
		return ".gif"
	}
	// WebP: 52 49 46 46 ... 57 45 42 50
	if len(data) >= 12 && data[0] == 0x52 && data[1] == 0x49 && data[8] == 0x57 && data[9] == 0x45 {
		return ".webp"
	}
	// BMP: 42 4D
	if data[0] == 0x42 && data[1] == 0x4D {
		return ".bmp"
	}
	return ".jpg" // default to jpg for WeChat images
}

func mimeTypeFromImageExt(ext string) string {
	switch strings.ToLower(ext) {
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	case ".bmp":
		return "image/bmp"
	default:
		return "application/octet-stream"
	}
}
