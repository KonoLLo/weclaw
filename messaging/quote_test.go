package messaging

import (
	"crypto/md5"
	"encoding/hex"
	"path/filepath"
	"testing"
	"time"

	"github.com/fastclaw-ai/weclaw/ilink"
)

func TestQuoteStorePersistsAndMergesMedia(t *testing.T) {
	path := filepath.Join(t.TempDir(), "quotes.json")
	store := &quoteStore{path: path, records: make(map[string]quoteRecord)}

	store.put(quoteRecord{
		ConversationID: "user-a",
		MessageID:      "100",
		Direction:      "inbound",
		Body:           "hello",
	})
	store.put(quoteRecord{
		ConversationID: "user-a",
		MessageID:      "100",
		Direction:      "inbound",
		MediaPath:      "/tmp/image.png",
		MediaMIME:      "image/png",
		MediaName:      "image.png",
		MediaSize:      123,
	})
	store.flush()

	loaded := &quoteStore{path: path, records: make(map[string]quoteRecord)}
	rec, ok := loaded.get("user-a", "100")
	if !ok {
		t.Fatal("expected persisted quote record")
	}
	if rec.Body != "hello" || rec.MediaPath != "/tmp/image.png" || rec.MediaMIME != "image/png" {
		t.Fatalf("unexpected merged record: %+v", rec)
	}
	if _, ok := loaded.get("user-b", "100"); ok {
		t.Fatal("record must be scoped to the conversation")
	}
}

func TestQuoteStoreExpiresOldRecords(t *testing.T) {
	path := filepath.Join(t.TempDir(), "quotes.json")
	store := &quoteStore{path: path, records: make(map[string]quoteRecord)}
	store.put(quoteRecord{
		ConversationID: "user-a",
		MessageID:      "200",
		Body:           "old",
		CreatedAt:      time.Now().Add(-quoteRetention - time.Hour).UnixMilli(),
	})
	if _, ok := store.get("user-a", "200"); ok {
		t.Fatal("expired record should not be returned")
	}
}

func TestResolvePartialQuoteWithHash(t *testing.T) {
	full := "第一段内容。第二段需要被引用。第三段内容。"
	selected := "第二段需要被引用。"
	sum := md5.Sum([]byte(selected))

	got := resolvePartialQuote(full, &ilink.PartialText{
		Start:      "第二段",
		End:        "引用。",
		StartIndex: 0,
		EndIndex:   0,
		QuoteMD5:   hex.EncodeToString(sum[:]),
	})
	if got != selected {
		t.Fatalf("resolvePartialQuote() = %q, want %q", got, selected)
	}
}

func TestResolvePartialQuoteFallsBackWithoutHash(t *testing.T) {
	full := "alpha beta alpha beta"
	got := resolvePartialQuote(full, &ilink.PartialText{
		Start:      "alpha",
		End:        "beta",
		StartIndex: 1,
		EndIndex:   1,
	})
	if got != "alpha beta" {
		t.Fatalf("resolvePartialQuote() = %q, want %q", got, "alpha beta")
	}
}

func TestQuoteInlineBodyUsesTitleAndItem(t *testing.T) {
	ref := &ilink.RefMessage{
		Title: "摘要",
		MessageItem: &ilink.MessageItem{
			Type:     ilink.ItemTypeText,
			TextItem: &ilink.TextItem{Text: "原文"},
		},
	}
	if got := quoteInlineBody(ref); got != "摘要\n原文" {
		t.Fatalf("quoteInlineBody() = %q", got)
	}
}
