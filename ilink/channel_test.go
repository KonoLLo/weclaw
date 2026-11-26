package ilink

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestConfigureChannelSanitizesAndBuildsRequests(t *testing.T) {
	old := channelConfig()
	t.Cleanup(func() { ConfigureChannel(old.botAgent, old.routeTag) })

	ConfigureChannel("  TestBot/1.0\nInjected  ", 42)
	if got := BuildBaseInfo().BotAgent; got != "TestBot/1.0Injected" {
		t.Fatalf("bot_agent = %q", got)
	}
	if got := withRouteTag("https://example.test/a?x=1"); got != "https://example.test/a?x=1&_cradle_hostname=g42" {
		t.Fatalf("route tag URL without query separator: %q", got)
	}
	if got := withRouteTag("https://example.test/a"); got != "https://example.test/a?_cradle_hostname=g42" {
		t.Fatalf("route tag URL: %q", got)
	}
}

func TestNotifyStartSendsChannelMetadata(t *testing.T) {
	old := channelConfig()
	t.Cleanup(func() { ConfigureChannel(old.botAgent, old.routeTag) })
	ConfigureChannel("TestBot/2.0", 9)

	var gotPath, gotTag, gotHostname, gotAgent string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotTag = r.Header.Get("SKRouteTag")
		gotHostname = r.URL.Query().Get("_cradle_hostname")
		var body struct {
			BaseInfo BaseInfo `json:"base_info"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotAgent = body.BaseInfo.BotAgent
		_, _ = w.Write([]byte(`{"ret":0}`))
	}))
	defer server.Close()

	client := &Client{baseURL: server.URL, httpClient: server.Client(), botToken: "token"}
	if err := client.NotifyStart(context.Background()); err != nil {
		t.Fatalf("NotifyStart() error = %v", err)
	}
	if gotPath != "/ilink/bot/msg/notifystart" {
		t.Fatalf("path = %q", gotPath)
	}
	if gotTag != "9" || gotHostname != "g9" {
		t.Fatalf("route tag header=%q hostname=%q", gotTag, gotHostname)
	}
	if gotAgent != "TestBot/2.0" {
		t.Fatalf("bot_agent = %q", gotAgent)
	}
	if strings.Contains(gotAgent, "\n") {
		t.Fatal("bot_agent must not contain control characters")
	}
}
