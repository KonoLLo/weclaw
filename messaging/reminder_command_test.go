package messaging

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fastclaw-ai/weclaw/agent"
)

func testReminderNow() time.Time {
	return time.Date(2026, 11, 26, 10, 0, 0, 0, time.Local)
}

func TestParseReminderOneShot(t *testing.T) {
	now := testReminderNow()
	cases := []struct {
		text        string
		wantContent string
		wantAt      string
	}{
		{"提醒我明天上午9点交报告", "交报告", "2026-11-27T09:00:00"},
		{"明天9点提醒我交报告", "交报告", "2026-11-27T09:00:00"},
		{"30分钟后提醒我喝水", "喝水", "2026-11-26T10:30:00"},
		{"半小时后提醒我关火", "关火", "2026-11-26T10:30:00"},
		{"一个半小时后提醒我取快递", "取快递", "2026-11-26T11:30:00"},
		{"3天后提醒我交报告", "交报告", "2026-11-29T10:00:00"},
		{"2周后提醒我复查", "复查", "2026-12-10T10:00:00"},
		{"十五分钟后提醒我喝水", "喝水", "2026-11-26T10:15:00"},
		{"提醒我十分钟后喝水", "喝水", "2026-11-26T10:10:00"},
		{"提醒我1小时后喝水", "喝水", "2026-11-26T11:00:00"},
		{"提醒我两小时后喝水", "喝水", "2026-11-26T12:00:00"},
		{"提醒我9点喝水", "喝水", "2026-11-27T09:00:00"},
		{"提醒我10点半开会", "开会", "2026-11-26T10:30:00"},
		{"提醒我晚上12点睡觉", "睡觉", "2026-11-27T00:00:00"},
		{"帮我提醒我明天9点交报告", "交报告", "2026-11-27T09:00:00"},
		{"设个提醒：30分钟后喝水", "喝水", "2026-11-26T10:30:00"},
		{"明天早上7点叫我起床", "起床", "2026-11-27T07:00:00"},
		{"明早7点叫我起床", "起床", "2026-11-27T07:00:00"},
		{"今晚8点叫我吃饭", "吃饭", "2026-11-26T20:00:00"},
		{"明晚8点提醒我开会", "开会", "2026-11-27T20:00:00"},
		{"提醒我明天9点钟交报告", "交报告", "2026-11-27T09:00:00"},
		{"提醒我明天9点请客", "请客", "2026-11-27T09:00:00"},
		{"提醒我明天9点一下雨就收衣服", "一下雨就收衣服", "2026-11-27T09:00:00"},
		{"提醒我明天9点给我妈打电话", "给我妈打电话", "2026-11-27T09:00:00"},
		{"提醒我明天9点记得带伞", "记得带伞", "2026-11-27T09:00:00"},
		{"提醒下我30分钟后喝水", "喝水", "2026-11-26T10:30:00"},
		{"一分钟后用一句话提醒我喝水", "喝水", "2026-11-26T10:01:00"},
		{"明天9点用聊天的语气提醒我交报告", "交报告", "2026-11-27T09:00:00"},
		{"简短提醒我30分钟后喝水", "喝水", "2026-11-26T10:30:00"},
		{"明天9点提醒我用一句话总结文章", "用一句话总结文章", "2026-11-27T09:00:00"},
	}
	for _, tc := range cases {
		req, ok := parseReminderRequestAt(tc.text, now)
		if !ok {
			t.Fatalf("%q was not recognized as a reminder", tc.text)
		}
		if req.Err != "" {
			t.Fatalf("%q returned error: %s", tc.text, req.Err)
		}
		if req.Content != tc.wantContent {
			t.Errorf("%q content = %q, want %q", tc.text, req.Content, tc.wantContent)
		}
		if want := "at:" + tc.wantAt; req.Schedule != want {
			t.Errorf("%q schedule = %q, want %q", tc.text, req.Schedule, want)
		}
		if !req.Next.Equal(time.Date(
			req.Next.Year(), req.Next.Month(), req.Next.Day(),
			req.Next.Hour(), req.Next.Minute(), req.Next.Second(), 0, req.Next.Location())) {
			t.Errorf("%q Next has sub-second precision: %v", tc.text, req.Next)
		}
	}
}

func TestFormatChattyReminder(t *testing.T) {
	cases := map[string]string{
		"喝水":     "到点啦，记得喝水。",
		"记得喝水":   "到点啦，记得喝水。",
		"别忘了带伞":  "到点啦，别忘了带伞。",
		"该交报告了":  "到点啦，该交报告了。",
		"交报告了":   "到点啦，该交报告了。",
		"交报告吧":   "到点啦，交报告吧。",
		"下午三点开会": "到点啦，记得下午三点开会。",
		"喝水。":    "到点啦，记得喝水。",
		"":       "到点啦，别忘了。",
	}
	for input, want := range cases {
		if got := formatChattyReminder(input); got != want {
			t.Errorf("formatChattyReminder(%q) = %q, want %q", input, got, want)
		}
	}
}

type stubReminderAgent struct {
	reply          string
	err            error
	conversationID string
	prompt         string
}

func (a *stubReminderAgent) Chat(_ context.Context, conversationID, message string) (string, error) {
	a.conversationID = conversationID
	a.prompt = message
	return a.reply, a.err
}

func (a *stubReminderAgent) ResetSession(context.Context, string) (string, error) {
	return "", nil
}

func (a *stubReminderAgent) Info() agent.AgentInfo { return agent.AgentInfo{} }

func (a *stubReminderAgent) SetCwd(string) {}

func TestReminderMessageUsesChatAgent(t *testing.T) {
	ag := &stubReminderAgent{reply: "该喝水啦，忙完记得补口水。"}
	s := NewCronScheduler(nil, nil, "", func(string) agent.Agent { return ag })

	got := s.reminderMessage(context.Background(), CronJob{ID: "r1", Message: "喝水"}, "u1", "喝水")
	if got != ag.reply {
		t.Fatalf("reminderMessage() = %q, want agent reply %q", got, ag.reply)
	}
	if ag.conversationID != "u1" {
		t.Errorf("agent conversation = %q, want u1", ag.conversationID)
	}
	if !strings.Contains(ag.prompt, "喝水") || !strings.Contains(ag.prompt, "聊天") {
		t.Errorf("agent prompt does not carry chat-style reminder context: %q", ag.prompt)
	}
}

func TestReminderMessageFallsBackWhenAgentFails(t *testing.T) {
	ag := &stubReminderAgent{err: errors.New("offline")}
	s := NewCronScheduler(nil, nil, "", func(string) agent.Agent { return ag })

	got := s.reminderMessage(context.Background(), CronJob{ID: "r1", Message: "喝水"}, "u1", "喝水")
	if want := formatChattyReminder("喝水"); got != want {
		t.Fatalf("reminderMessage() = %q, want fallback %q", got, want)
	}
}

func TestParseReminderShorthandTomorrowAtSixAM(t *testing.T) {
	now := time.Date(2026, 11, 26, 6, 0, 0, 0, time.Local)
	req, ok := parseReminderRequestAt("明早7点叫我起床", now)
	if !ok || req.Err != "" {
		t.Fatalf("明早7点 failed: ok=%v err=%q", ok, req.Err)
	}
	if req.Content != "起床" || req.Schedule != "at:2026-11-27T07:00:00" {
		t.Fatalf("明早7点 parsed as content=%q schedule=%q", req.Content, req.Schedule)
	}
}

func TestParseReminderRepeating(t *testing.T) {
	now := testReminderNow()
	cases := []struct {
		text        string
		wantContent string
		wantCron    string
		wantNext    string
	}{
		{"每天下午5点提醒我写日报", "写日报", "0 17 * * *", "2026-11-26T17:00:00"},
		{"提醒我每天下午5点写日报", "写日报", "0 17 * * *", "2026-11-26T17:00:00"},
		{"每周一上午10点提醒我开会", "开会", "0 10 * * 1", "2026-11-30T10:00:00"},
		{"每个工作日下午6点提醒我下班", "下班", "0 18 * * 1-5", "2026-11-26T18:00:00"},
	}
	for _, tc := range cases {
		req, ok := parseReminderRequestAt(tc.text, now)
		if !ok {
			t.Fatalf("%q was not recognized as a reminder", tc.text)
		}
		if req.Err != "" {
			t.Fatalf("%q returned error: %s", tc.text, req.Err)
		}
		if req.Content != tc.wantContent {
			t.Errorf("%q content = %q, want %q", tc.text, req.Content, tc.wantContent)
		}
		if req.Schedule != tc.wantCron {
			t.Errorf("%q schedule = %q, want %q", tc.text, req.Schedule, tc.wantCron)
		}
		want, err := time.ParseInLocation("2006-01-02T15:04:05", tc.wantNext, time.Local)
		if err != nil {
			t.Fatal(err)
		}
		if !req.Next.Equal(want) {
			t.Errorf("%q next = %v, want %v", tc.text, req.Next, want)
		}
	}
}

func TestParseReminderRejectsBadInput(t *testing.T) {
	now := testReminderNow()
	for _, tc := range []struct {
		text    string
		wantErr string
	}{
		{"提醒我今天上午9点喝水", "已经过了"},
		{"提醒我今早8点喝水", "已经过了"},
		{"提醒我明天上午9点", "做什么"},
		{"提醒我25点喝水", "钟点"},
		{"提醒我下周一下午3点开会", "还没支持"},
		{"提醒我周五下午3点开会", "还没支持"},
		{"提醒我明天9点开会讨论10点方案", "只定一个时间"},
		{"提醒我每天下午5点", "做什么"},
		{"提醒我30分钟后9点喝水", "只认一个时间"},
		{"提醒我每天30分钟后喝水", "不能再加"},
		{"提醒我明晚12点睡觉", "晚上12点"},
		{"提醒我明天晚上12点睡觉", "晚上12点"},
	} {
		req, ok := parseReminderRequestAt(tc.text, now)
		if !ok {
			t.Fatalf("%q should be recognized as reminder intent", tc.text)
		}
		if req.Err == "" || !strings.Contains(req.Err, tc.wantErr) {
			t.Errorf("%q error = %q, want containing %q", tc.text, req.Err, tc.wantErr)
		}
	}
}

func TestParseReminderIntentGuard(t *testing.T) {
	now := testReminderNow()
	if _, ok := parseReminderRequestAt("这个提醒功能明天上线", now); ok {
		t.Fatal("ordinary mention of 提醒 was claimed as a reminder")
	}
	if _, ok := parseReminderRequestAt("提醒我们明天9点开会", now); ok {
		t.Fatal("提醒我们 should not be treated as 提醒我")
	}
}

func TestDirectReminderWithoutTargetRecordsFailure(t *testing.T) {
	store := NewCronStore(filepath.Join(t.TempDir(), "jobs.json"))
	job := CronJob{
		ID:       "rm1",
		Name:     "提醒：喝水",
		Enabled:  true,
		Schedule: "every:1h",
		Message:  "喝水",
		UserID:   "u",
		Direct:   true,
		State:    JobState{NextRunAt: time.Now().Add(-time.Minute)},
	}
	if err := store.Add(job); err != nil {
		t.Fatal(err)
	}
	s := NewCronScheduler(store, nil, "", nil)
	s.executeJob(context.Background(), job)

	got, ok := store.Get("rm1")
	if !ok {
		t.Fatal("job disappeared")
	}
	if got.State.RunCount != 1 || got.State.LastStatus != "error" {
		t.Fatalf("direct reminder did not record its no-target failure: %+v", got.State)
	}
}

func TestParseReminderChineseNumbers(t *testing.T) {
	cases := map[string]int{
		"一": 1, "两": 2, "十": 10, "十五": 15, "二十": 20, "二十三": 23, "零": 0,
	}
	for raw, want := range cases {
		got, ok := parseReminderNumber(raw)
		if !ok || got != want {
			t.Errorf("parseReminderNumber(%q) = %d, %v; want %d, true", raw, got, ok, want)
		}
	}
	for _, raw := range []string{"", "一百", "abc"} {
		if _, ok := parseReminderNumber(raw); ok {
			t.Errorf("parseReminderNumber(%q) unexpectedly succeeded", raw)
		}
	}
}
