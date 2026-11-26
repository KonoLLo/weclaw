package messaging

import (
	"strings"
	"testing"
	"time"

	"github.com/fastclaw-ai/weclaw/agent"
)

func TestBuildSearchReplyHasNoLinks(t *testing.T) {
	results := []agent.SearchResult{{Title: "Trip", URL: "https://www.trip.com/flights/"}}
	out := buildSearchReply("上海到北京的机票", "上海到北京有直飞航班，票价随日期变化。", results, "DuckDuckGo", 3*time.Second)
	if strings.Contains(out, "http") {
		t.Fatalf("reply contains URL: %s", out)
	}
	if !strings.Contains(out, "上海到北京的机票") || !strings.Contains(out, "直飞航班") {
		t.Fatalf("missing content: %s", out)
	}
	if !strings.Contains(out, "DuckDuckGo") {
		t.Fatalf("missing provider: %s", out)
	}
}