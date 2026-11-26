package ilink

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

const (
	maxConsecutiveFailures = 5
	initialBackoff         = 3 * time.Second
	maxBackoff             = 60 * time.Second
	sessionExpiredBackoff  = 5 * time.Second
	errCodeSessionExpired  = -14

	inboundSeenTTL      = 24 * time.Hour
	inboundSeenMax      = 10_000
	inboundPendingLimit = 1_000
)

var (
	inboundDebounceMS atomic.Int64
	inboundMaxBatch   atomic.Int64
)

func init() {
	inboundDebounceMS.Store(800)
	inboundMaxBatch.Store(8)
}

// ConfigureInbound sets the per-sender merge window and maximum batch size.
// Non-positive values keep the defaults (800ms, 8 messages).
func ConfigureInbound(debounceMS, maxBatch int) {
	if debounceMS <= 0 {
		debounceMS = 800
	}
	if debounceMS > 10_000 {
		debounceMS = 10_000
	}
	if maxBatch <= 0 {
		maxBatch = 8
	}
	if maxBatch > 50 {
		maxBatch = 50
	}
	inboundDebounceMS.Store(int64(debounceMS))
	inboundMaxBatch.Store(int64(maxBatch))
	log.Printf("[monitor] inbound merge: debounce=%dms max_batch=%d", debounceMS, maxBatch)
}

func currentInboundDebounce() time.Duration {
	return time.Duration(inboundDebounceMS.Load()) * time.Millisecond
}

func currentInboundMaxBatch() int {
	v := inboundMaxBatch.Load()
	if v < 1 {
		return 1
	}
	return int(v)
}

// MessageHandler is called for each received message.
type MessageHandler func(ctx context.Context, client *Client, msg WeixinMessage)

// MessageBatchHandler receives a contiguous group from one sender after the
// merge window closes. Messages are delivered in arrival order.
type MessageBatchHandler func(ctx context.Context, client *Client, msgs []WeixinMessage)

type pendingMessage struct {
	Message   WeixinMessage `json:"message"`
	ArrivedAt int64         `json:"arrived_at"`
	Immediate bool          `json:"immediate,omitempty"`
}

type inFlightBatch struct {
	ID        string           `json:"id"`
	UserID    string           `json:"user_id"`
	Messages  []pendingMessage `json:"messages"`
	StartedAt int64            `json:"started_at"`
}

type syncData struct {
	GetUpdatesBuf string           `json:"get_updates_buf"`
	Pending       []pendingMessage `json:"pending,omitempty"`
	InFlight      []inFlightBatch  `json:"in_flight,omitempty"`
	Seen          []string         `json:"seen,omitempty"`
}

// Monitor manages the long-poll loop for receiving messages.
type Monitor struct {
	client        *Client
	handler       MessageHandler
	batchHandler  MessageBatchHandler
	getUpdatesBuf string
	bufPath       string
	failures      int
	lastActivity  time.Time

	stateMu       sync.Mutex
	pending       map[string][]pendingMessage
	inFlight      map[string]inFlightBatch
	seen          map[string]time.Time
	queued        map[string]struct{}
	urgentQueue   []WeixinMessage
	urgentHandler MessageHandler

	workerMu sync.Mutex
	workers  map[string]chan struct{}
	workerWG sync.WaitGroup
}

// NewMonitor creates a new long-poll monitor.
func NewMonitor(client *Client, handler MessageHandler) (*Monitor, error) {
	return newMonitor(client, handler, nil)
}

// NewMonitorWithBatch creates a monitor that delivers merged batches.
func NewMonitorWithBatch(client *Client, handler MessageBatchHandler) (*Monitor, error) {
	return newMonitor(client, nil, handler)
}

// SetUrgentHandler installs a callback for commands that must run while the
// sender's normal queue is busy. A nil handler disables the bypass.
func (m *Monitor) SetUrgentHandler(handler MessageHandler) {
	m.stateMu.Lock()
	m.urgentHandler = handler
	m.stateMu.Unlock()
}

func newMonitor(client *Client, handler MessageHandler, batchHandler MessageBatchHandler) (*Monitor, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	accountID := NormalizeAccountID(client.BotID())
	bufPath := filepath.Join(home, ".weclaw", "accounts", accountID+".sync.json")

	m := &Monitor{
		client:       client,
		handler:      handler,
		batchHandler: batchHandler,
		bufPath:      bufPath,
		lastActivity: time.Now(),
		pending:      make(map[string][]pendingMessage),
		inFlight:     make(map[string]inFlightBatch),
		seen:         make(map[string]time.Time),
		queued:       make(map[string]struct{}),
		workers:      make(map[string]chan struct{}),
	}
	m.loadState()
	return m, nil
}

// Run starts the long-poll loop. It blocks until ctx is cancelled.
// Automatically recovers from errors with exponential backoff.
func (m *Monitor) Run(ctx context.Context) error {
	log.Println("[monitor] starting long-poll loop")
	m.startPendingWorkers(ctx)

	for {
		select {
		case <-ctx.Done():
			log.Println("[monitor] shutting down")
			return ctx.Err()
		default:
		}

		resp, err := m.client.GetUpdates(ctx, m.getUpdatesBuf)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			m.failures++
			backoff := m.calcBackoff()
			log.Printf("[monitor] GetUpdates error (%d/%d, backoff=%s): %v",
				m.failures, maxConsecutiveFailures, backoff, err)
			if m.failures == maxConsecutiveFailures {
				log.Printf("[monitor] WARNING: %d consecutive failures. If this persists, run `weclaw login` to re-authenticate.", maxConsecutiveFailures)
			}
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return ctx.Err()
			}
			continue
		}

		// Reset failure counter on any successful response
		m.failures = 0
		m.lastActivity = time.Now()

		// Session expired — reset sync buf and reconnect silently
		if resp.ErrCode == errCodeSessionExpired {
			if m.getUpdatesBuf != "" {
				log.Printf("[monitor] session expired, resetting sync buf")
				m.stateMu.Lock()
				m.getUpdatesBuf = ""
				_ = m.saveStateLocked()
				m.stateMu.Unlock()
				if err := ClearContextTokens(m.client.BotID()); err != nil {
					log.Printf("[monitor] failed to clear context tokens: %v", err)
				}
			} else {
				// Sync buf already empty but still getting session expired:
				// the bot token itself has expired. The user needs to re-login.
				log.Printf("[monitor] WARNING: WeChat session expired and cannot be auto-recovered. Run `weclaw login` to re-authenticate.")
			}
			select {
			case <-time.After(sessionExpiredBackoff):
			case <-ctx.Done():
				return ctx.Err()
			}
			continue
		}

		// Other server errors
		if resp.Ret != 0 && resp.ErrCode != 0 {
			log.Printf("[monitor] server error: ret=%d errcode=%d errmsg=%s", resp.Ret, resp.ErrCode, resp.ErrMsg)
			continue
		}

		users := m.acceptMessages(resp.GetUpdatesBuf, resp.Msgs)
		m.dispatchUrgent(ctx)
		for _, userID := range users {
			m.ensureWorker(ctx, userID)
			m.wakeWorker(userID)
		}
	}
}

// calcBackoff returns an exponential backoff duration capped at maxBackoff.
func (m *Monitor) calcBackoff() time.Duration {
	d := initialBackoff
	for i := 1; i < m.failures; i++ {
		d *= 2
		if d > maxBackoff {
			return maxBackoff
		}
	}
	return d
}

func (m *Monitor) loadState() {
	data, err := os.ReadFile(m.bufPath)
	if err != nil {
		return
	}
	var s syncData
	if json.Unmarshal(data, &s) != nil {
		return
	}
	m.getUpdatesBuf = s.GetUpdatesBuf

	now := time.Now()
	for _, key := range s.Seen {
		if key != "" {
			m.seen[key] = now
		}
	}
	for _, pm := range s.Pending {
		key := messageKey(pm.Message)
		if key == "" || pm.Message.FromUserID == "" {
			continue
		}
		if _, ok := m.seen[key]; ok {
			continue
		}
		if _, ok := m.queued[key]; ok {
			continue
		}
		m.pending[pm.Message.FromUserID] = append(m.pending[pm.Message.FromUserID], pm)
		m.queued[key] = struct{}{}
	}
	if len(s.InFlight) > 0 {
		for _, batch := range s.InFlight {
			log.Printf("[monitor] WARNING: discarding interrupted in-flight batch %s for %s (%d message(s)); duplicate side effects are avoided by not replaying it", batch.ID, batch.UserID, len(batch.Messages))
			for _, pm := range batch.Messages {
				key := messageKey(pm.Message)
				if key != "" {
					m.seen[key] = now
					delete(m.queued, key)
				}
			}
		}
	}
	m.pruneSeenLocked(now)
	if s.GetUpdatesBuf != "" {
		log.Printf("[monitor] loaded sync state from %s (pending=%d seen=%d)", m.bufPath, len(s.Pending), len(m.seen))
	}
}

func (m *Monitor) acceptMessages(nextBuf string, msgs []WeixinMessage) []string {
	now := time.Now()
	m.stateMu.Lock()
	oldBuf := m.getUpdatesBuf
	if nextBuf != "" {
		m.getUpdatesBuf = nextBuf
	}

	userSet := make(map[string]struct{})
	changed := false
	for _, msg := range msgs {
		if msg.MessageType != MessageTypeUser || msg.MessageState != MessageStateFinish {
			continue
		}
		if msg.FromUserID == "" {
			continue
		}
		key := messageKey(msg)
		if key == "" {
			continue
		}
		if _, ok := m.seen[key]; ok {
			continue
		}
		if _, ok := m.queued[key]; ok {
			continue
		}
		if m.urgentHandler != nil && isStopCommand(msg) {
			m.seen[key] = now
			m.urgentQueue = append(m.urgentQueue, msg)
			changed = true
			continue
		}
		if len(m.pending[msg.FromUserID]) >= inboundPendingLimit {
			log.Printf("[monitor] WARNING: pending queue full for %s, dropping oldest message", msg.FromUserID)
			dropped := m.pending[msg.FromUserID][0]
			m.pending[msg.FromUserID] = m.pending[msg.FromUserID][1:]
			delete(m.queued, messageKey(dropped.Message))
		}
		m.pending[msg.FromUserID] = append(m.pending[msg.FromUserID], pendingMessage{
			Message:   msg,
			ArrivedAt: now.UnixMilli(),
			Immediate: isLikelyCommand(msg),
		})
		m.queued[key] = struct{}{}
		userSet[msg.FromUserID] = struct{}{}
		changed = true
	}

	if nextBuf != "" || changed {
		if err := m.saveStateLocked(); err != nil {
			log.Printf("[monitor] failed to persist inbound state: %v", err)
			if nextBuf != "" {
				m.getUpdatesBuf = oldBuf
			}
		}
	}
	m.stateMu.Unlock()

	users := make([]string, 0, len(userSet))
	for userID := range userSet {
		users = append(users, userID)
	}
	sort.Strings(users)
	return users
}

func isStopCommand(msg WeixinMessage) bool {
	for _, item := range msg.ItemList {
		if item.Type == ItemTypeText && item.TextItem != nil {
			return strings.EqualFold(strings.TrimSpace(item.TextItem.Text), "/stop")
		}
	}
	return false
}

func (m *Monitor) dispatchUrgent(ctx context.Context) {
	m.stateMu.Lock()
	handler := m.urgentHandler
	urgent := m.urgentQueue
	m.urgentQueue = nil
	m.stateMu.Unlock()
	if handler == nil {
		return
	}
	for _, msg := range urgent {
		go handler(ctx, m.client, msg)
	}
}

func (m *Monitor) startPendingWorkers(ctx context.Context) {
	m.stateMu.Lock()
	users := make([]string, 0, len(m.pending))
	for userID, list := range m.pending {
		if len(list) > 0 {
			users = append(users, userID)
		}
	}
	m.stateMu.Unlock()
	for _, userID := range users {
		m.ensureWorker(ctx, userID)
		m.wakeWorker(userID)
	}
	if len(users) > 0 {
		log.Printf("[monitor] replaying pending messages for %d sender(s)", len(users))
	}
}

func (m *Monitor) ensureWorker(ctx context.Context, userID string) {
	m.workerMu.Lock()
	defer m.workerMu.Unlock()
	if _, ok := m.workers[userID]; ok {
		return
	}
	wake := make(chan struct{}, 1)
	m.workers[userID] = wake
	m.workerWG.Add(1)
	go m.worker(ctx, userID, wake)
}

type batchPollResult int

const (
	batchNone batchPollResult = iota
	batchReady
	batchWait
)

func (m *Monitor) worker(ctx context.Context, userID string, wake chan struct{}) {
	defer m.workerWG.Done()
	defer func() {
		m.workerMu.Lock()
		delete(m.workers, userID)
		m.workerMu.Unlock()
	}()

	for {
		batch, result, wait := m.tryTakeBatch(userID)
		switch result {
		case batchReady:
			m.processBatch(ctx, batch)
		case batchWait:
			timer := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-wake:
				timer.Stop()
			case <-timer.C:
			}
		default:
			select {
			case <-ctx.Done():
				return
			case <-wake:
			}
		}
	}
}

func (m *Monitor) tryTakeBatch(userID string) (inFlightBatch, batchPollResult, time.Duration) {
	now := time.Now()
	m.stateMu.Lock()
	defer m.stateMu.Unlock()

	list := m.pending[userID]
	if len(list) == 0 {
		return inFlightBatch{}, batchNone, 0
	}

	for i, pm := range list {
		if !pm.Immediate {
			continue
		}
		batch := m.beginBatchLocked(userID, []pendingMessage{pm}, now)
		remaining := make([]pendingMessage, 0, len(list)-1)
		remaining = append(remaining, list[:i]...)
		remaining = append(remaining, list[i+1:]...)
		m.pending[userID] = remaining
		return batch, batchReady, 0
	}

	maxBatch := currentInboundMaxBatch()
	if len(list) >= maxBatch {
		batch := m.beginBatchLocked(userID, clonePending(list[:maxBatch]), now)
		m.pending[userID] = clonePending(list[maxBatch:])
		return batch, batchReady, 0
	}

	newest := list[len(list)-1].ArrivedAt
	wait := currentInboundDebounce() - time.Duration(now.UnixMilli()-newest)*time.Millisecond
	if wait > 0 {
		return inFlightBatch{}, batchWait, wait
	}

	batch := m.beginBatchLocked(userID, clonePending(list), now)
	m.pending[userID] = nil
	return batch, batchReady, 0
}

func clonePending(list []pendingMessage) []pendingMessage {
	out := make([]pendingMessage, len(list))
	copy(out, list)
	return out
}

func (m *Monitor) beginBatchLocked(userID string, msgs []pendingMessage, now time.Time) inFlightBatch {
	batch := inFlightBatch{
		ID:        uuid.NewString(),
		UserID:    userID,
		Messages:  msgs,
		StartedAt: now.UnixMilli(),
	}
	m.inFlight[batch.ID] = batch
	if err := m.saveStateLocked(); err != nil {
		log.Printf("[monitor] failed to persist in-flight batch %s: %v", batch.ID, err)
	}
	return batch
}

func (m *Monitor) processBatch(ctx context.Context, batch inFlightBatch) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[monitor] panic while processing batch %s: %v", batch.ID, r)
		}
		m.finishBatch(batch)
	}()

	msgs := make([]WeixinMessage, 0, len(batch.Messages))
	for _, pm := range batch.Messages {
		msgs = append(msgs, pm.Message)
	}
	if len(msgs) == 0 {
		return
	}
	log.Printf("[monitor] processing batch %s for %s (%d message(s))", batch.ID, batch.UserID, len(msgs))

	if m.batchHandler != nil {
		m.batchHandler(ctx, m.client, msgs)
		return
	}
	if m.handler != nil {
		for _, msg := range msgs {
			if ctx.Err() != nil {
				return
			}
			m.handler(ctx, m.client, msg)
		}
	}
}

func (m *Monitor) finishBatch(batch inFlightBatch) {
	now := time.Now()
	m.stateMu.Lock()
	defer m.stateMu.Unlock()
	delete(m.inFlight, batch.ID)
	for _, pm := range batch.Messages {
		key := messageKey(pm.Message)
		if key == "" {
			continue
		}
		delete(m.queued, key)
		m.seen[key] = now
	}
	m.pruneSeenLocked(now)
	if err := m.saveStateLocked(); err != nil {
		log.Printf("[monitor] failed to persist completed batch %s: %v", batch.ID, err)
	}
}

func (m *Monitor) wakeWorker(userID string) {
	m.workerMu.Lock()
	wake := m.workers[userID]
	m.workerMu.Unlock()
	if wake == nil {
		return
	}
	select {
	case wake <- struct{}{}:
	default:
	}
}

func (m *Monitor) pruneSeenLocked(now time.Time) {
	cutoff := now.Add(-inboundSeenTTL)
	for key, at := range m.seen {
		if at.Before(cutoff) {
			delete(m.seen, key)
		}
	}
	if len(m.seen) <= inboundSeenMax {
		return
	}
	type seenEntry struct {
		key string
		at  time.Time
	}
	entries := make([]seenEntry, 0, len(m.seen))
	for key, at := range m.seen {
		entries = append(entries, seenEntry{key: key, at: at})
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].at.Before(entries[j].at)
	})
	for _, entry := range entries[:len(entries)-inboundSeenMax] {
		delete(m.seen, entry.key)
	}
}

func (m *Monitor) saveStateLocked() error {
	state := syncData{GetUpdatesBuf: m.getUpdatesBuf}
	for _, list := range m.pending {
		state.Pending = append(state.Pending, list...)
	}
	for _, batch := range m.inFlight {
		state.InFlight = append(state.InFlight, batch)
	}
	for key := range m.seen {
		state.Seen = append(state.Seen, key)
	}

	dir := filepath.Dir(m.bufPath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	tmp := m.bufPath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, m.bufPath)
}

func messageKey(msg WeixinMessage) string {
	if msg.MessageID != 0 {
		return fmt.Sprintf("id:%d", msg.MessageID)
	}
	if id := strings.TrimSpace(msg.ClientID); id != "" {
		return "cid:" + id
	}
	if msg.CreateTimeMs != 0 || msg.Seq != 0 {
		return fmt.Sprintf("meta:%s:%d:%d", msg.FromUserID, msg.CreateTimeMs, msg.Seq)
	}
	return ""
}

func isLikelyCommand(msg WeixinMessage) bool {
	for _, item := range msg.ItemList {
		if item.Type != ItemTypeText || item.TextItem == nil {
			continue
		}
		text := strings.TrimSpace(item.TextItem.Text)
		return strings.HasPrefix(text, "/") || strings.HasPrefix(text, "@")
	}
	return false
}

// FormatMessageSummary returns a short description of a message for logging.
func FormatMessageSummary(msg WeixinMessage) string {
	text := ""
	for _, item := range msg.ItemList {
		if item.Type == ItemTypeText && item.TextItem != nil {
			text = item.TextItem.Text
			break
		}
	}
	if len(text) > 50 {
		text = text[:50] + "..."
	}
	return fmt.Sprintf("from=%s type=%d state=%d text=%q", msg.FromUserID, msg.MessageType, msg.MessageState, text)
}
