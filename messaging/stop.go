package messaging

import (
	"context"
	"errors"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/fastclaw-ai/weclaw/ilink"
)

const stopAbortTimeout = 5 * time.Second

type runningTask struct {
	cancel  context.CancelFunc
	abort   func(context.Context)
	label   string
	started time.Time
}

// beginTask registers a cancellable turn so /stop can reach it while the
// sender's normal worker is blocked on this task.
func (h *Handler) beginTask(ctx context.Context, userID, label string, abort func(context.Context)) (context.Context, func()) {
	if ctx == nil {
		ctx = context.Background()
	}
	taskCtx, cancel := context.WithCancel(ctx)

	h.tasksMu.Lock()
	if h.tasks == nil {
		h.tasks = make(map[string]map[uint64]runningTask)
	}
	h.taskSeq++
	id := h.taskSeq
	if h.tasks[userID] == nil {
		h.tasks[userID] = make(map[uint64]runningTask)
	}
	h.tasks[userID][id] = runningTask{cancel: cancel, abort: abort, label: label, started: time.Now()}
	h.tasksMu.Unlock()

	var once sync.Once
	finish := func() {
		once.Do(func() {
			h.tasksMu.Lock()
			if tasks := h.tasks[userID]; tasks != nil {
				delete(tasks, id)
				if len(tasks) == 0 {
					delete(h.tasks, userID)
				}
			}
			h.tasksMu.Unlock()
			cancel()
		})
	}
	return taskCtx, finish
}

func (h *Handler) stopUserTasks(userID string) []string {
	h.tasksMu.Lock()
	tasks := h.tasks[userID]
	delete(h.tasks, userID)
	h.tasksMu.Unlock()

	labels := make([]string, 0, len(tasks))
	seen := make(map[string]struct{}, len(tasks))
	for _, task := range tasks {
		task.cancel()
		if task.abort != nil {
			abort := task.abort
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), stopAbortTimeout)
				defer cancel()
				abort(ctx)
			}()
		}
		if task.label == "" {
			continue
		}
		if _, ok := seen[task.label]; ok {
			continue
		}
		seen[task.label] = struct{}{}
		labels = append(labels, task.label)
	}
	return labels
}

func isTaskCanceled(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// HandleUrgentMessage is installed on the iLink monitor so /stop bypasses the
// per-sender worker queue and can interrupt a running turn.
func (h *Handler) HandleUrgentMessage(ctx context.Context, client *ilink.Client, msg ilink.WeixinMessage) {
	if msg.MessageType != ilink.MessageTypeUser || msg.MessageState != ilink.MessageStateFinish {
		return
	}
	if msg.FromUserID == "" || (len(h.allowedUsers) > 0 && !h.isAllowed(msg.FromUserID)) {
		return
	}
	if !strings.EqualFold(strings.TrimSpace(extractText(msg)), "/stop") {
		return
	}
	h.handleStopCommand(ctx, client, msg, "")
}

func (h *Handler) handleStopCommand(ctx context.Context, client *ilink.Client, msg ilink.WeixinMessage, clientID string) {
	labels := h.stopUserTasks(msg.FromUserID)
	var reply string
	switch len(labels) {
	case 0:
		reply = "当前没有正在运行的任务。"
	case 1:
		reply = "已停止：" + labels[0]
	default:
		reply = "已停止：" + strings.Join(labels, "、")
	}
	log.Printf("[stop] %s stopped %d task(s): %v", msg.FromUserID, len(labels), labels)
	if err := SendTextReply(ctx, client, msg.FromUserID, reply, msg.ContextToken, clientID); err != nil {
		log.Printf("[stop] failed to send reply to %s: %v", msg.FromUserID, err)
	}
}

func canceledReply(err error) bool {
	// Agent deadlines are real failures; only an explicit /stop suppresses the
	// reply. Task context cancellation is normalized to context.Canceled by the
	// callers before this check.
	return errors.Is(err, context.Canceled)
}

func taskError(taskCtx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if taskCtx != nil && taskCtx.Err() != nil {
		return context.Canceled
	}
	return err
}
