package messaging

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fastclaw-ai/weclaw/ilink"
)

const (
	quoteRetention     = 30 * 24 * time.Hour
	quoteMaxRecords    = 10_000
	quoteMaxBodyRunes  = 16_000
	quoteFlushDebounce = 500 * time.Millisecond
)

type quoteRecord struct {
	ConversationID string `json:"conversation_id"`
	MessageID      string `json:"message_id"`
	Direction      string `json:"direction,omitempty"`
	Body           string `json:"body,omitempty"`
	MediaPath      string `json:"media_path,omitempty"`
	MediaMIME      string `json:"media_mime,omitempty"`
	MediaName      string `json:"media_name,omitempty"`
	MediaSize      int64  `json:"media_size,omitempty"`
	CreatedAt      int64  `json:"created_at"`
}

type quoteStoreFile struct {
	Records []quoteRecord `json:"records"`
}

type quoteStore struct {
	path    string
	mu      sync.Mutex
	records map[string]quoteRecord
	loaded  bool
	dirty   bool
	timer   *time.Timer
}

var quoteStores sync.Map // botID -> *quoteStore

func quoteStorePath(botID string) (string, error) {
	dir, err := ilink.AccountsDir()
	if err != nil {
		return "", err
	}
	name := ilink.NormalizeAccountID(botID)
	if name == "" {
		name = "default"
	}
	return filepath.Join(dir, name+".quotes.json"), nil
}

func getQuoteStore(botID string) *quoteStore {
	if store, ok := quoteStores.Load(botID); ok {
		return store.(*quoteStore)
	}
	path, err := quoteStorePath(botID)
	if err != nil {
		log.Printf("[quote] failed to resolve cache path: %v", err)
		return nil
	}
	store := &quoteStore{path: path, records: make(map[string]quoteRecord)}
	actual, _ := quoteStores.LoadOrStore(botID, store)
	return actual.(*quoteStore)
}

func quoteKey(conversationID, messageID string) string {
	return conversationID + "\x00" + messageID
}

func (s *quoteStore) loadLocked(now time.Time) {
	if s.loaded {
		return
	}
	s.loaded = true
	raw, err := os.ReadFile(s.path)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("[quote] failed to read %s: %v", s.path, err)
		}
		return
	}
	var data quoteStoreFile
	if err := json.Unmarshal(raw, &data); err != nil {
		log.Printf("[quote] ignoring corrupt cache %s: %v", s.path, err)
		return
	}
	cutoff := now.Add(-quoteRetention).UnixMilli()
	changed := false
	for _, rec := range data.Records {
		if rec.ConversationID == "" || rec.MessageID == "" {
			continue
		}
		if rec.CreatedAt < cutoff {
			changed = true
			continue
		}
		s.records[quoteKey(rec.ConversationID, rec.MessageID)] = rec
	}
	if len(s.records) > quoteMaxRecords {
		s.pruneLocked(now)
		changed = true
	}
	if changed {
		s.dirty = true
		s.scheduleFlushLocked()
	}
}

func (s *quoteStore) pruneLocked(now time.Time) {
	cutoff := now.Add(-quoteRetention).UnixMilli()
	kept := make(map[string]quoteRecord, len(s.records))
	for key, rec := range s.records {
		if rec.CreatedAt >= cutoff {
			kept[key] = rec
		}
	}
	if len(kept) > quoteMaxRecords {
		var oldestKey string
		var oldest int64
		for len(kept) > quoteMaxRecords {
			oldest = 0
			for key, rec := range kept {
				if oldest == 0 || rec.CreatedAt < oldest {
					oldest = rec.CreatedAt
					oldestKey = key
				}
			}
			delete(kept, oldestKey)
		}
	}
	s.records = kept
}

func (s *quoteStore) put(rec quoteRecord) {
	if rec.ConversationID == "" || rec.MessageID == "" {
		return
	}
	if rec.Body == "" && rec.MediaPath == "" {
		return
	}
	if runes := []rune(rec.Body); len(runes) > quoteMaxBodyRunes {
		rec.Body = string(runes[:quoteMaxBodyRunes])
	}
	now := time.Now()
	if rec.CreatedAt <= 0 {
		rec.CreatedAt = now.UnixMilli()
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadLocked(now)
	key := quoteKey(rec.ConversationID, rec.MessageID)
	if old, ok := s.records[key]; ok {
		if rec.Body == "" {
			rec.Body = old.Body
		}
		if rec.MediaPath == "" {
			rec.MediaPath = old.MediaPath
			rec.MediaMIME = old.MediaMIME
			rec.MediaName = old.MediaName
			rec.MediaSize = old.MediaSize
		}
		if rec.Direction == "" {
			rec.Direction = old.Direction
		}
	}
	s.records[key] = rec
	if len(s.records) > quoteMaxRecords {
		s.pruneLocked(now)
	}
	s.dirty = true
	s.scheduleFlushLocked()
}

func (s *quoteStore) get(conversationID, messageID string) (quoteRecord, bool) {
	if conversationID == "" || messageID == "" {
		return quoteRecord{}, false
	}
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadLocked(now)
	key := quoteKey(conversationID, messageID)
	rec, ok := s.records[key]
	if !ok {
		return quoteRecord{}, false
	}
	if now.Sub(time.UnixMilli(rec.CreatedAt)) > quoteRetention {
		delete(s.records, key)
		s.dirty = true
		s.scheduleFlushLocked()
		return quoteRecord{}, false
	}
	return rec, true
}

func (s *quoteStore) scheduleFlushLocked() {
	if s.timer != nil {
		return
	}
	s.timer = time.AfterFunc(quoteFlushDebounce, func() {
		s.mu.Lock()
		s.timer = nil
		s.mu.Unlock()
		s.flush()
	})
}

func (s *quoteStore) flush() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.dirty {
		return
	}
	records := make([]quoteRecord, 0, len(s.records))
	for _, rec := range s.records {
		records = append(records, rec)
	}
	raw, err := json.Marshal(quoteStoreFile{Records: records})
	if err != nil {
		log.Printf("[quote] failed to encode cache: %v", err)
		return
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		log.Printf("[quote] failed to create cache dir: %v", err)
		return
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		log.Printf("[quote] failed to write cache: %v", err)
		return
	}
	if err := os.Rename(tmp, s.path); err != nil {
		log.Printf("[quote] failed to replace cache: %v", err)
		return
	}
	s.dirty = false
}

// rememberQuoteMessage records an inbound or outbound text message under every
// ID the quoting client might send back.
func rememberQuoteMessage(botID, conversationID, direction, body string, messageIDs ...string) {
	body = strings.TrimSpace(body)
	if body == "" {
		return
	}
	store := getQuoteStore(botID)
	if store == nil {
		return
	}
	seen := make(map[string]struct{}, len(messageIDs))
	for _, id := range messageIDs {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		store.put(quoteRecord{
			ConversationID: conversationID,
			MessageID:      id,
			Direction:      direction,
			Body:           body,
			CreatedAt:      time.Now().UnixMilli(),
		})
	}
}

func rememberQuoteMedia(botID, conversationID, direction, mediaPath, mime, name string, size int64, messageIDs ...string) {
	if mediaPath == "" {
		return
	}
	store := getQuoteStore(botID)
	if store == nil {
		return
	}
	seen := make(map[string]struct{}, len(messageIDs))
	for _, id := range messageIDs {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		store.put(quoteRecord{
			ConversationID: conversationID,
			MessageID:      id,
			Direction:      direction,
			MediaPath:      mediaPath,
			MediaMIME:      mime,
			MediaName:      name,
			MediaSize:      size,
			CreatedAt:      time.Now().UnixMilli(),
		})
	}
}

func lookupQuoteMessage(botID, conversationID, messageID string) (quoteRecord, bool) {
	store := getQuoteStore(botID)
	if store == nil {
		return quoteRecord{}, false
	}
	return store.get(conversationID, messageID)
}

func rememberInboundMedia(botID string, msg ilink.WeixinMessage, mediaPath, mime, name string) {
	if mediaPath == "" {
		return
	}
	var size int64
	if info, err := os.Stat(mediaPath); err == nil {
		size = info.Size()
	}
	rememberQuoteMedia(botID, msg.FromUserID, "inbound", mediaPath, mime, name, size, quoteIDsForMessage(msg)...)
}

func quoteIDsForMessage(msg ilink.WeixinMessage) []string {
	ids := make([]string, 0, len(msg.ItemList)+1)
	if msg.MessageID != 0 {
		ids = append(ids, fmt.Sprintf("%d", msg.MessageID))
	}
	for _, item := range msg.ItemList {
		if id := strings.TrimSpace(item.MsgID); id != "" {
			ids = append(ids, id)
		}
	}
	return ids
}
