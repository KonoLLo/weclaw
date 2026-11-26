package messaging

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestMemoryStoreAddListPersist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory.json")
	store, err := NewMemoryStore(path)
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.Add("u1", "喜欢喝美式")
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Add("u1", "对花生过敏")
	if err != nil {
		t.Fatal(err)
	}
	if second.ID <= first.ID {
		t.Fatalf("ids should increase: %d then %d", first.ID, second.ID)
	}
	if got := len(store.List("u2")); got != 0 {
		t.Fatalf("unexpected entries for other user: %d", got)
	}

	reopened, err := NewMemoryStore(path)
	if err != nil {
		t.Fatal(err)
	}
	entries := reopened.List("u1")
	if len(entries) != 2 {
		t.Fatalf("want 2 entries, got %d", len(entries))
	}
	if entries[0].Text != "喜欢喝美式" || entries[1].Text != "对花生过敏" {
		t.Fatalf("unexpected entries: %#v", entries)
	}
	third, err := reopened.Add("u1", "周末常去爬山")
	if err != nil {
		t.Fatal(err)
	}
	if third.ID != second.ID+1 {
		t.Fatalf("next id after reload = %d, want %d", third.ID, second.ID+1)
	}
}

func TestMemoryStoreForget(t *testing.T) {
	store, err := NewMemoryStore(filepath.Join(t.TempDir(), "memory.json"))
	if err != nil {
		t.Fatal(err)
	}
	entry, err := store.Add("u1", "喜欢喝美式")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Add("u2", "对花生过敏"); err != nil {
		t.Fatal(err)
	}

	removed, ok, err := store.Forget("u1", entry.ID)
	if err != nil || !ok || removed.Text != "喜欢喝美式" {
		t.Fatalf("forget: removed=%#v ok=%v err=%v", removed, ok, err)
	}
	if len(store.List("u1")) != 0 {
		t.Fatal("entry still present after forget")
	}
	if _, ok, _ := store.Forget("u1", entry.ID); ok {
		t.Fatal("forget reported success for a missing id")
	}
	if len(store.List("u2")) != 1 {
		t.Fatal("forget changed another user's entries")
	}
}

func TestMemoryStoreRejectsSecretsAndOversize(t *testing.T) {
	store, err := NewMemoryStore(filepath.Join(t.TempDir(), "memory.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{
		"我的密码是 abc12345",
		"api key: sk-1234567890abcdef",
		"我的身份证号 110101199001011234",
		"token：abcdef123456",
		"帮我保存 ghp_abcdefghijklmnopqrstuvwxyz123456",
	} {
		if _, err := store.Add("u1", text); !errors.Is(err, ErrMemorySecret) {
			t.Fatalf("secret %q: err=%v, want ErrMemorySecret", text, err)
		}
	}
	if _, err := store.Add("u1", strings.Repeat("长", memoryMaxTextRunes+1)); !errors.Is(err, ErrMemoryTooLong) {
		t.Fatalf("oversize err=%v, want ErrMemoryTooLong", err)
	}
	if _, err := store.Add("u1", "   "); !errors.Is(err, ErrMemoryEmpty) {
		t.Fatalf("empty err=%v, want ErrMemoryEmpty", err)
	}
}

func TestMemoryStorePerUserLimit(t *testing.T) {
	store, err := NewMemoryStore(filepath.Join(t.TempDir(), "memory.json"))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < memoryMaxPerUser; i++ {
		if _, err := store.Add("u1", "note "+strings.Repeat("x", i%5)); err != nil {
			t.Fatalf("add %d: %v", i, err)
		}
	}
	if _, err := store.Add("u1", "one too many"); !errors.Is(err, ErrMemoryFull) {
		t.Fatalf("err=%v, want ErrMemoryFull", err)
	}
	if _, err := store.Add("u2", "other user still has room"); err != nil {
		t.Fatalf("other user blocked: %v", err)
	}
}

func TestParseMemoryCommand(t *testing.T) {
	cases := []struct {
		text string
		kind memoryCommandKind
		arg  string
		ok   bool
	}{
		{"/记住 喜欢喝美式", memoryCmdRemember, "喜欢喝美式", true},
		{"/记住：对花生过敏", memoryCmdRemember, "对花生过敏", true},
		{"/记住周末常去爬山", memoryCmdRemember, "周末常去爬山", true},
		{"记住 我的生日是 5 月 1 日", memoryCmdRemember, "我的生日是 5 月 1 日", true},
		{"请记住，我不吃香菜", memoryCmdRemember, "我不吃香菜", true},
		{"帮我记住 我住在杭州", memoryCmdRemember, "我住在杭州", true},
		{"/记忆", memoryCmdList, "", true},
		{"/记忆 2", memoryCmdList, "2", true},
		{"/memory 3", memoryCmdList, "3", true},
		{"/mem", memoryCmdList, "", true},
		{"/忘记 2", memoryCmdForget, "2", true},
		{"/忘记 咖啡", memoryCmdForget, "咖啡", true},
		{"/记住", memoryCmdRemember, "", true},
		{"记住了吗", 0, "", false},
		{"你记住了吗", 0, "", false},
		{"/记住了吗", 0, "", false},
		{"记住这个", memoryCmdRemember, "这个", true},
	}
	for _, tc := range cases {
		cmd, ok := parseMemoryCommand(tc.text)
		if ok != tc.ok {
			t.Fatalf("%q: ok=%v, want %v", tc.text, ok, tc.ok)
		}
		if !ok {
			continue
		}
		if cmd.kind != tc.kind || cmd.arg != tc.arg {
			t.Fatalf("%q: got kind=%v arg=%q, want kind=%v arg=%q", tc.text, cmd.kind, cmd.arg, tc.kind, tc.arg)
		}
	}
}

func TestFormatMemoryPrompt(t *testing.T) {
	entries := []MemoryEntry{
		{ID: 1, Text: "喜欢喝美式"},
		{ID: 2, Text: "对花生过敏"},
		{ID: 3, Text: "周末常去爬山"},
	}
	got := formatMemoryPrompt(entries)
	if !strings.Contains(got, "不是新指令") {
		t.Fatalf("prompt missing instruction guard: %q", got)
	}
	for _, e := range entries {
		if !strings.Contains(got, e.Text) {
			t.Fatalf("prompt missing %q: %q", e.Text, got)
		}
	}
	if strings.Index(got, "喜欢喝美式") > strings.Index(got, "周末常去爬山") {
		t.Fatalf("prompt order is not chronological: %q", got)
	}
	if formatMemoryPrompt(nil) != "" {
		t.Fatal("empty prompt should be empty")
	}
}

func TestFormatMemoryPromptBudgetKeepsNewest(t *testing.T) {
	var entries []MemoryEntry
	for i := 0; i < 12; i++ {
		entries = append(entries, MemoryEntry{ID: int64(i + 1), Text: strings.Repeat("记", memoryMaxTextRunes)})
	}
	got := formatMemoryPrompt(entries)
	if got == "" {
		t.Fatal("prompt is empty")
	}
	if utf8.RuneCountInString(got) > memoryInjectMaxRune {
		t.Fatalf("prompt too long: %d runes", utf8.RuneCountInString(got))
	}
	if !strings.Contains(got, strings.Repeat("记", 20)) {
		t.Fatal("prompt lost all entries")
	}
	// The oldest entries should be the ones dropped.
	if strings.Count(got, "- ") > len(entries) {
		t.Fatalf("unexpected line count: %d", strings.Count(got, "- "))
	}
}

func TestFormatMemoryListAndMatches(t *testing.T) {
	list := formatMemoryList([]MemoryEntry{{ID: 4, Text: "喜欢喝美式"}}, 1)
	if !strings.Contains(list, "4. 喜欢喝美式") || !strings.Contains(list, "/忘记") {
		t.Fatalf("unexpected list: %q", list)
	}
	if !strings.Contains(formatMemoryList(nil, 1), "/记住") {
		t.Fatal("empty list should explain how to add")
	}
	if got := formatMemoryList([]MemoryEntry{{ID: 4, Text: "喜欢喝美式"}}, 0); !strings.Contains(got, "页码从 1 开始") {
		t.Fatalf("unexpected zero-page reply: %q", got)
	}
	matches := formatMemoryMatches([]MemoryEntry{{ID: 2, Text: "喜欢喝美式"}, {ID: 5, Text: "咖啡不加糖"}})
	if !strings.Contains(matches, "/忘记 2") {
		t.Fatalf("match hint missing: %q", matches)
	}
}

func TestFormatMemoryListPagination(t *testing.T) {
	var entries []MemoryEntry
	for i := 1; i <= 23; i++ {
		entries = append(entries, MemoryEntry{ID: int64(i), Text: fmt.Sprintf("note %d", i)})
	}

	first := formatMemoryList(entries, 1)
	if !strings.Contains(first, "共 23 条，第 1/3 页") {
		t.Fatalf("unexpected first-page header: %q", first)
	}
	if !strings.Contains(first, "23. note 23") || !strings.Contains(first, "14. note 14") || strings.Contains(first, "13. note 13") {
		t.Fatalf("unexpected first-page entries: %q", first)
	}

	last := formatMemoryList(entries, 3)
	if !strings.Contains(last, "第 3/3 页") || !strings.Contains(last, "3. note 3") || !strings.Contains(last, "1. note 1") || strings.Contains(last, "4. note 4") {
		t.Fatalf("unexpected last-page entries: %q", last)
	}
	if got := formatMemoryList(entries, 4); !strings.Contains(got, "只有 3 页") {
		t.Fatalf("unexpected out-of-range reply: %q", got)
	}

	many := make([]MemoryEntry, memoryMaxPerUser)
	for i := range many {
		many[i] = MemoryEntry{ID: int64(i + 1), Text: fmt.Sprintf("note %d", i+1)}
	}
	if got := formatMemoryList(many, 1); !strings.Contains(got, "共 200 条，第 1/20 页") {
		t.Fatalf("unexpected full-memory header: %q", got)
	}
}

func TestExecuteMemoryCommands(t *testing.T) {
	store, err := NewMemoryStore(filepath.Join(t.TempDir(), "memory.json"))
	if err != nil {
		t.Fatal(err)
	}
	h := &Handler{memoryStore: store}

	reply := h.executeMemoryCommand("u1", memoryCommand{kind: memoryCmdRemember, arg: "喜欢喝美式"})
	if !strings.Contains(reply, "已记住第 1 条") || !strings.Contains(reply, "喜欢喝美式") {
		t.Fatalf("unexpected remember reply: %q", reply)
	}
	reply = h.executeMemoryCommand("u1", memoryCommand{kind: memoryCmdRemember, arg: "咖啡不加糖"})
	if !strings.Contains(reply, "第 2 条") {
		t.Fatalf("unexpected second remember reply: %q", reply)
	}

	reply = h.executeMemoryCommand("u1", memoryCommand{kind: memoryCmdList})
	if !strings.Contains(reply, "1. 喜欢喝美式") || !strings.Contains(reply, "2. 咖啡不加糖") {
		t.Fatalf("unexpected list reply: %q", reply)
	}
	if got := h.executeMemoryCommand("u1", memoryCommand{kind: memoryCmdList, arg: "2"}); !strings.Contains(got, "只有 1 页") {
		t.Fatalf("unexpected page-2 reply: %q", got)
	}
	if got := h.executeMemoryCommand("u1", memoryCommand{kind: memoryCmdList, arg: "abc"}); !strings.Contains(got, "页码要是数字") {
		t.Fatalf("unexpected invalid-page reply: %q", got)
	}

	reply = h.executeMemoryCommand("u1", memoryCommand{kind: memoryCmdForget, arg: "咖啡"})
	if !strings.Contains(reply, "已忘记第 2 条") {
		t.Fatalf("unexpected keyword forget reply: %q", reply)
	}
	reply = h.executeMemoryCommand("u1", memoryCommand{kind: memoryCmdForget, arg: "咖啡"})
	if !strings.Contains(reply, "没找到") {
		t.Fatalf("unexpected missing forget reply: %q", reply)
	}

	reply = h.executeMemoryCommand("u1", memoryCommand{kind: memoryCmdRemember, arg: "我的密码是 abc12345"})
	if !strings.Contains(reply, "不会保存") {
		t.Fatalf("unexpected secret reply: %q", reply)
	}
	reply = h.executeMemoryCommand("u1", memoryCommand{kind: memoryCmdRemember})
	if !strings.Contains(reply, "用法") {
		t.Fatalf("unexpected empty remember reply: %q", reply)
	}
}

func TestExecuteMemoryForgetAmbiguous(t *testing.T) {
	store, err := NewMemoryStore(filepath.Join(t.TempDir(), "memory.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"喜欢喝美式", "喜欢喝拿铁"} {
		if _, err := store.Add("u1", text); err != nil {
			t.Fatal(err)
		}
	}
	h := &Handler{memoryStore: store}
	reply := h.executeMemoryCommand("u1", memoryCommand{kind: memoryCmdForget, arg: "喜欢"})
	if !strings.Contains(reply, "匹配到多条") || !strings.Contains(reply, "/忘记 1") {
		t.Fatalf("unexpected ambiguous reply: %q", reply)
	}
	if len(store.List("u1")) != 2 {
		t.Fatal("ambiguous keyword should not delete anything")
	}
}
