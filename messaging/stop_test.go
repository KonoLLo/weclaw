package messaging

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/fastclaw-ai/weclaw/agent"
	"github.com/fastclaw-ai/weclaw/ilink"
)

type blockingAgent struct {
	started chan struct{}
}

func (a *blockingAgent) Chat(ctx context.Context, conversationID, message string) (string, error) {
	close(a.started)
	<-ctx.Done()
	return "", ctx.Err()
}

func (a *blockingAgent) ResetSession(context.Context, string) (string, error) { return "", nil }
func (a *blockingAgent) Info() agent.AgentInfo                                { return agent.AgentInfo{Name: "fake", Type: "cli"} }
func (a *blockingAgent) SetCwd(string)                                        {}

func TestStopUserTasksCancelsAndAborts(t *testing.T) {
	h := &Handler{}
	ctx, finish := h.beginTask(context.Background(), "user-a", "代码任务", func(stopCtx context.Context) {
		select {
		case <-stopCtx.Done():
		case <-time.After(time.Second):
		}
	})
	finish()
	if ctx.Err() == nil {
		t.Fatal("finish should cancel the task context")
	}

	aborted := make(chan struct{}, 1)
	taskCtx, finish2 := h.beginTask(context.Background(), "user-a", "代码任务", func(stopCtx context.Context) {
		aborted <- struct{}{}
	})
	defer finish2()
	labels := h.stopUserTasks("user-a")
	if len(labels) != 1 || labels[0] != "代码任务" {
		t.Fatalf("labels = %v", labels)
	}
	select {
	case <-taskCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("stop should cancel the task context")
	}
	select {
	case <-aborted:
	case <-time.After(time.Second):
		t.Fatal("stop should invoke the agent abort hook")
	}
	h.tasksMu.Lock()
	remaining := len(h.tasks["user-a"])
	h.tasksMu.Unlock()
	if remaining != 0 {
		t.Fatalf("tasks were not removed: %d", remaining)
	}
}

func TestTaskErrorNormalizesCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := taskError(ctx, errors.New("prompt error")); !errors.Is(got, context.Canceled) {
		t.Fatalf("taskError = %v, want context.Canceled", got)
	}
	if got := taskError(context.Background(), errors.New("boom")); got == nil || got.Error() != "boom" {
		t.Fatalf("taskError = %v, want original error", got)
	}
}

func TestIsTaskCanceled(t *testing.T) {
	if !isTaskCanceled(context.Canceled) || !isTaskCanceled(context.DeadlineExceeded) {
		t.Fatal("context cancellation should count as task cancellation")
	}
	if isTaskCanceled(errors.New("boom")) {
		t.Fatal("ordinary errors must not count as cancellation")
	}
}

func TestCanceledReplyOnlyHidesExplicitStop(t *testing.T) {
	if !canceledReply(context.Canceled) {
		t.Fatal("explicit cancellation should suppress the error reply")
	}
	if canceledReply(context.DeadlineExceeded) {
		t.Fatal("agent deadlines must still be reported to the user")
	}
}

func TestChatWithAgentCanBeStopped(t *testing.T) {
	h := &Handler{}
	ag := &blockingAgent{started: make(chan struct{})}
	done := make(chan error, 1)
	go func() {
		_, err := h.chatWithAgent(context.Background(), ag, nil, ilink.WeixinMessage{FromUserID: "user-a"}, "/slow")
		done <- err
	}()
	select {
	case <-ag.started:
	case <-time.After(time.Second):
		t.Fatal("agent was not called")
	}
	labels := h.stopUserTasks("user-a")
	if len(labels) != 1 || labels[0] != "AI 回复" {
		t.Fatalf("labels = %v", labels)
	}
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("chatWithAgent error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("stopped agent turn did not return")
	}
}
