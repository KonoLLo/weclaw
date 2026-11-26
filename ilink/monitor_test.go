package ilink

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func testMonitor(t *testing.T) *Monitor {
	t.Helper()
	return &Monitor{
		bufPath:  filepath.Join(t.TempDir(), "account.sync.json"),
		pending:  make(map[string][]pendingMessage),
		inFlight: make(map[string]inFlightBatch),
		seen:     make(map[string]time.Time),
		queued:   make(map[string]struct{}),
		workers:  make(map[string]chan struct{}),
	}
}

func textMessage(id int64, user, text string) WeixinMessage {
	return WeixinMessage{
		MessageID:    id,
		FromUserID:   user,
		MessageType:  MessageTypeUser,
		MessageState: MessageStateFinish,
		ItemList: []MessageItem{{
			Type:     ItemTypeText,
			TextItem: &TextItem{Text: text},
		}},
	}
}

func TestStopCommandBypassesSenderQueue(t *testing.T) {
	m := testMonitor(t)
	got := make(chan WeixinMessage, 1)
	m.SetUrgentHandler(func(ctx context.Context, client *Client, msg WeixinMessage) {
		got <- msg
	})

	users := m.acceptMessages("buf-stop", []WeixinMessage{textMessage(41, "user-a", "/stop")})
	if len(users) != 0 {
		t.Fatalf("stop command should not wake sender worker, got users=%v", users)
	}
	if len(m.pending["user-a"]) != 0 {
		t.Fatalf("stop command was queued: %+v", m.pending["user-a"])
	}
	if _, ok := m.seen["id:41"]; !ok {
		t.Fatal("stop command should be marked seen")
	}

	m.dispatchUrgent(context.Background())
	select {
	case msg := <-got:
		if msg.MessageID != 41 {
			t.Fatalf("urgent message id = %d", msg.MessageID)
		}
	case <-time.After(time.Second):
		t.Fatal("urgent handler was not called")
	}
}

func TestAcceptMessagesPersistsAndDeduplicates(t *testing.T) {
	m := testMonitor(t)
	msg := textMessage(1, "user-a", "hello")

	users := m.acceptMessages("buf-1", []WeixinMessage{msg})
	if len(users) != 1 || users[0] != "user-a" {
		t.Fatalf("acceptMessages users = %v", users)
	}
	if got := len(m.pending["user-a"]); got != 1 {
		t.Fatalf("pending count = %d, want 1", got)
	}
	if m.getUpdatesBuf != "buf-1" {
		t.Fatalf("getUpdatesBuf = %q", m.getUpdatesBuf)
	}

	users = m.acceptMessages("buf-1", []WeixinMessage{msg})
	if len(users) != 0 || len(m.pending["user-a"]) != 1 {
		t.Fatalf("duplicate message was queued again: users=%v pending=%d", users, len(m.pending["user-a"]))
	}

	raw, err := os.ReadFile(m.bufPath)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	var state syncData
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatalf("parse state: %v", err)
	}
	if state.GetUpdatesBuf != "buf-1" || len(state.Pending) != 1 {
		t.Fatalf("persisted state = %+v", state)
	}
}

func TestTryTakeBatchWaitsForDebounceAndMerges(t *testing.T) {
	oldDebounce := inboundDebounceMS.Load()
	oldMax := inboundMaxBatch.Load()
	ConfigureInbound(40, 8)
	defer func() {
		inboundDebounceMS.Store(oldDebounce)
		inboundMaxBatch.Store(oldMax)
	}()

	m := testMonitor(t)
	m.pending["user-a"] = []pendingMessage{
		{Message: textMessage(1, "user-a", "one"), ArrivedAt: time.Now().UnixMilli()},
	}

	if _, result, wait := m.tryTakeBatch("user-a"); result != batchWait || wait <= 0 {
		t.Fatalf("first poll result=%v wait=%s, want batchWait", result, wait)
	}

	m.pending["user-a"] = append(m.pending["user-a"], pendingMessage{
		Message:   textMessage(2, "user-a", "two"),
		ArrivedAt: time.Now().UnixMilli(),
	})
	time.Sleep(60 * time.Millisecond)
	batch, result, _ := m.tryTakeBatch("user-a")
	if result != batchReady {
		t.Fatalf("result = %v, want batchReady", result)
	}
	if len(batch.Messages) != 2 || len(m.pending["user-a"]) != 0 {
		t.Fatalf("batch=%d remaining=%d", len(batch.Messages), len(m.pending["user-a"]))
	}
	if len(m.inFlight) != 1 {
		t.Fatalf("in-flight batches = %d, want 1", len(m.inFlight))
	}
}

func TestCommandBypassesDebounce(t *testing.T) {
	oldDebounce := inboundDebounceMS.Load()
	oldMax := inboundMaxBatch.Load()
	ConfigureInbound(5_000, 8)
	defer func() {
		inboundDebounceMS.Store(oldDebounce)
		inboundMaxBatch.Store(oldMax)
	}()

	m := testMonitor(t)
	m.acceptMessages("", []WeixinMessage{textMessage(10, "user-a", "/new")})
	batch, result, wait := m.tryTakeBatch("user-a")
	if result != batchReady || wait != 0 {
		t.Fatalf("result=%v wait=%s, want immediate batch", result, wait)
	}
	if len(batch.Messages) != 1 || batch.Messages[0].Message.MessageID != 10 {
		t.Fatalf("unexpected immediate batch: %+v", batch.Messages)
	}
}

func TestLoadStateDropsInterruptedBatch(t *testing.T) {
	m := testMonitor(t)
	state := syncData{
		GetUpdatesBuf: "buf",
		Pending: []pendingMessage{{
			Message:   textMessage(21, "user-a", "pending"),
			ArrivedAt: time.Now().UnixMilli(),
		}},
		InFlight: []inFlightBatch{{
			ID:     "batch-1",
			UserID: "user-a",
			Messages: []pendingMessage{{
				Message:   textMessage(22, "user-a", "inflight"),
				ArrivedAt: time.Now().UnixMilli(),
			}},
		}},
	}
	raw, _ := json.Marshal(state)
	if err := os.WriteFile(m.bufPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	m.loadState()
	if len(m.pending["user-a"]) != 1 || m.pending["user-a"][0].Message.MessageID != 21 {
		t.Fatalf("pending = %+v", m.pending["user-a"])
	}
	if _, ok := m.seen["id:22"]; !ok {
		t.Fatal("interrupted batch should be marked seen")
	}
	if len(m.inFlight) != 0 {
		t.Fatalf("in-flight batch should be dropped, got %d", len(m.inFlight))
	}
}

func TestFinishBatchMarksSeenAndClearsQueue(t *testing.T) {
	m := testMonitor(t)
	m.acceptMessages("", []WeixinMessage{textMessage(31, "user-a", "done")})
	m.pending["user-a"][0].ArrivedAt = time.Now().Add(-time.Second).UnixMilli()
	batch, result, _ := m.tryTakeBatch("user-a")
	if result != batchReady {
		t.Fatalf("result = %v", result)
	}
	m.finishBatch(batch)
	if _, ok := m.seen["id:31"]; !ok {
		t.Fatal("finished message should be marked seen")
	}
	if _, ok := m.queued["id:31"]; ok {
		t.Fatal("finished message should leave the queued set")
	}
	if len(m.inFlight) != 0 {
		t.Fatalf("in-flight batch was not cleared: %d", len(m.inFlight))
	}
}
