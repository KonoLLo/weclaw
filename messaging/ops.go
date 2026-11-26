package messaging

import (
	"context"
	"log"
	"sync/atomic"

	"github.com/fastclaw-ai/weclaw/ilink"
)

var errorNoticesEnabled atomic.Bool

// ConfigureErrorNotices toggles user-facing failure notices. Errors are always
// logged; when disabled, transient media failures stay out of the chat.
func ConfigureErrorNotices(enabled bool) {
	errorNoticesEnabled.Store(enabled)
	log.Printf("[handler] user-facing error notices: %t", enabled)
}

func (h *Handler) sendFailureNotice(ctx context.Context, client *ilink.Client, msg ilink.WeixinMessage, text, clientID string) {
	if !errorNoticesEnabled.Load() {
		log.Printf("[handler] failure notice suppressed for %s: %s", msg.FromUserID, text)
		return
	}
	if err := SendTextReply(ctx, client, msg.FromUserID, text, msg.ContextToken, clientID); err != nil {
		log.Printf("[handler] failed to send failure notice to %s: %v", msg.FromUserID, err)
	}
}
