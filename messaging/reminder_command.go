package messaging

import (
	"context"
	"fmt"
	"log"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/fastclaw-ai/weclaw/ilink"
	"github.com/google/uuid"
)

const (
	reminderMaxContentRunes = 200
	reminderMaxDuration     = 10 * 365 * 24 * time.Hour
)

var (
	reminderIntentPhrases = []string{
		"记得提醒我",
		"帮我提醒我",
		"请提醒我",
		"提醒我一下",
		"提醒一下我",
		"提醒下我",
		"提醒我",
		"记得叫我",
		"叫我",
		"喊我",
		"设置提醒",
		"设个提醒",
		"定个提醒",
		"加个提醒",
		"创建一个提醒",
		"创建提醒",
		"提醒一下",
	}

	reminderStylePhrases = []string{
		"用聊天那种语气",
		"用聊天的语气",
		"像聊天那样",
		"像聊天一样",
		"聊天语气",
		"聊天口吻",
		"用一句话",
		"一句话",
		"用一句",
		"简短一点",
		"简短说",
		"简短",
		"简单一点",
		"简单说",
		"口语化",
		"用口语",
		"随便一点",
		"自然一点",
		"温柔一点",
		"轻松一点",
	}

	reminderClockRe = regexp.MustCompile(`(?:在|于)?\s*(今天|明天|后天|大后天|今早|明早|今晚|明晚)?\s*(凌晨|早上|上午|中午|下午|傍晚|晚上|夜里)?\s*(\d{1,2}|[一二两三四五六七八九十]+)\s*(?:点|时|[:：])\s*(?:钟|整)?\s*(半|\d{1,2}|[一二两三四五六七八九十]+)?\s*分?`)

	reminderRelativeHalfRe = regexp.MustCompile(`(?:在|于)?\s*(?:再过|过)?\s*(?:(\d+|[一二两三四五六七八九十]+)\s*(?:个)?\s*)?半\s*(?:个)?\s*(小时|钟头|天|日|周|星期)\s*(?:后|以后|之后|过后)`)
	reminderRelativeRe     = regexp.MustCompile(`(?:在|于)?\s*(?:再过|过)?\s*(\d+|[一二两三四五六七八九十]+)\s*(?:个)?\s*(秒钟|秒|分钟|分|小时|钟头|天|日|周|星期)\s*(?:后|以后|之后|过后)`)

	reminderRepeatRe = regexp.MustCompile(`每\s*(天|个工作日|工作日|(?:周|星期)[一二三四五六日天1-7])`)

	reminderUnsupportedDateRe = regexp.MustCompile(`\d{4}\s*[-/年]|\d{1,2}\s*月\s*\d{1,2}\s*[日号]|(?:下|这|本)?\s*(?:周|星期)[一二三四五六日天1-7]|(?:下|这|本)\s*个?\s*月|周末`)
)

type reminderRequest struct {
	Content     string
	Schedule    string
	Next        time.Time
	RepeatLabel string
	Err         string
}

func parseReminderRequest(text string) (reminderRequest, bool) {
	return parseReminderRequestAt(text, time.Now())
}

// parseReminderRequestAt parses a Chinese reminder phrase around a supplied
// clock so tests are deterministic. It only claims messages that contain a
// reminder intent; ordinary mentions of "提醒" are left to the agent.
func parseReminderRequestAt(text string, now time.Time) (reminderRequest, bool) {
	original := strings.TrimSpace(text)
	start, end, ok := findReminderIntent(original)
	if !ok {
		return reminderRequest{}, false
	}
	prefix := original[:start]
	for _, phrase := range reminderStylePhrases {
		prefix = strings.ReplaceAll(prefix, phrase, " ")
	}
	stripped := strings.TrimSpace(prefix + " " + original[end:])
	if stripped == "" {
		return reminderRequest{Err: reminderTimeUsage()}, true
	}

	var repeatSpan [2]int
	repeatKind := ""
	if m := reminderRepeatRe.FindStringSubmatchIndex(stripped); m != nil {
		repeatSpan = [2]int{m[0], m[1]}
		whole := stripped[m[2]:m[3]]
		switch {
		case whole == "天":
			repeatKind = "daily"
		case strings.Contains(whole, "工作日"):
			repeatKind = "workday"
		default:
			runes := []rune(whole)
			if len(runes) > 0 {
				repeatKind = "weekly:" + string(runes[len(runes)-1])
			}
		}
	}
	if repeatKind == "" && reminderUnsupportedDateRe.MatchString(stripped) {
		return reminderRequest{Err: "这个日期说法本鱼还没支持。换成“明天早上 9 点”“3 天后”“每天下午 5 点”这种说法试试。"}, true
	}

	relative := reminderRelativeHalfRe.FindStringSubmatchIndex(stripped)
	half := true
	if relative == nil {
		relative = reminderRelativeRe.FindStringSubmatchIndex(stripped)
		half = false
	}
	clock := reminderClockRe.FindStringSubmatchIndex(stripped)

	if repeatKind != "" {
		if relative != nil {
			return reminderRequest{Err: "重复提醒里不能再加“N 分钟后”。换成“每天下午 5 点”这种说法。"}, true
		}
		if clock == nil {
			return reminderRequest{Err: reminderTimeUsage()}, true
		}
		hour, minute, dateToken, err := parseReminderClock(stripped, clock)
		if err != nil {
			return reminderRequest{Err: err.Error()}, true
		}
		if dateToken != "" {
			return reminderRequest{Err: "重复提醒里不要带“今天/明天”。"}, true
		}
		schedule, label, err := buildReminderRepeatSchedule(repeatKind, hour, minute)
		if err != nil {
			return reminderRequest{Err: err.Error()}, true
		}
		content := reminderContent(stripped, repeatSpan, [2]int{clock[0], clock[1]})
		if content == "" {
			return reminderRequest{Err: reminderContentUsage()}, true
		}
		if reminderContentHasAnotherTime(content) {
			return reminderRequest{Err: "一次只定一个时间，提醒内容里又出现了时间。"}, true
		}
		next, err := NewCronScheduler(nil, nil, "", nil).ComputeNextRun(schedule, now)
		if err != nil {
			return reminderRequest{Err: "提醒时间算不出来：" + err.Error()}, true
		}
		return reminderRequest{Content: content, Schedule: schedule, Next: next, RepeatLabel: label}, true
	}

	if relative != nil {
		if clock != nil {
			return reminderRequest{Err: "一次只认一个时间。把“30 分钟后”和具体钟点只留一个。"}, true
		}
		at, err := reminderRelativeTime(stripped, relative, half, now)
		if err != nil {
			return reminderRequest{Err: err.Error()}, true
		}
		content := reminderContent(stripped, [2]int{relative[0], relative[1]})
		if content == "" {
			return reminderRequest{Err: reminderContentUsage()}, true
		}
		if reminderContentHasAnotherTime(content) {
			return reminderRequest{Err: "一次只定一个时间，提醒内容里又出现了时间。"}, true
		}
		return reminderRequest{
			Content:  content,
			Schedule: "at:" + at.Format("2006-01-02T15:04:05"),
			Next:     at,
		}, true
	}

	if clock == nil {
		return reminderRequest{Err: reminderTimeUsage()}, true
	}
	hour, minute, dateToken, err := parseReminderClock(stripped, clock)
	if err != nil {
		return reminderRequest{Err: err.Error()}, true
	}
	at, err := reminderClockTime(now, hour, minute, dateToken)
	if err != nil {
		return reminderRequest{Err: err.Error()}, true
	}
	content := reminderContent(stripped, [2]int{clock[0], clock[1]})
	if content == "" {
		return reminderRequest{Err: reminderContentUsage()}, true
	}
	if reminderContentHasAnotherTime(content) {
		return reminderRequest{Err: "一次只定一个时间，提醒内容里又出现了时间。"}, true
	}
	return reminderRequest{
		Content:  content,
		Schedule: "at:" + at.Format("2006-01-02T15:04:05"),
		Next:     at,
	}, true
}

func findReminderIntent(text string) (int, int, bool) {
	for _, phrase := range reminderIntentPhrases {
		idx := strings.Index(text, phrase)
		if idx < 0 {
			continue
		}
		after := idx + len(phrase)
		if strings.HasSuffix(phrase, "我") && strings.HasPrefix(text[after:], "们") {
			continue
		}
		return idx, after, true
	}
	return 0, 0, false
}

func parseReminderClock(text string, m []int) (hour, minute int, dateToken string, err error) {
	dateToken = reminderSubmatch(text, m, 1)
	period := reminderSubmatch(text, m, 2)
	hourToken := reminderSubmatch(text, m, 3)
	minuteToken := reminderSubmatch(text, m, 4)

	hour, ok := parseReminderNumber(hourToken)
	if !ok || hour < 0 || hour > 23 {
		return 0, 0, "", fmt.Errorf("钟点“%s”没看懂，用 0-23 点试试。", hourToken)
	}
	if minuteToken != "" && minuteToken != "半" &&
		isSingleCNNumeral(minuteToken) && !strings.Contains(text[m[9]:m[1]], "分") &&
		m[9] < len(text) {
		next, _ := utf8.DecodeRuneInString(text[m[9]:])
		if !reminderCleanBoundary(string(next)) {
			minuteToken = ""
			m[1] = m[8]
		}
	}
	if minuteToken == "" {
		minute = 0
	} else if minuteToken == "半" {
		minute = 30
	} else {
		minute, ok = parseReminderNumber(minuteToken)
		if !ok || minute < 0 || minute > 59 {
			return 0, 0, "", fmt.Errorf("分钟“%s”没看懂，用 0-59 分试试。", minuteToken)
		}
	}
	if period != "" && hour > 12 {
		return 0, 0, "", fmt.Errorf("%s%s 点这个说法不常见，换个 24 小时时间试试。", period, hourToken)
	}

	switch dateToken {
	case "今早":
		dateToken = "今天"
		if period == "" {
			period = "早上"
		}
	case "明早":
		dateToken = "明天"
		if period == "" {
			period = "早上"
		}
	case "今晚":
		dateToken = "今天"
		if period == "" {
			period = "晚上"
		}
	case "明晚":
		dateToken = "明天"
		if period == "" {
			period = "晚上"
		}
	}
	if (period == "傍晚" || period == "晚上" || period == "夜里") && hour == 12 && dateToken != "" {
		return 0, 0, "", fmt.Errorf("“晚上12点”容易和当天0点混。想指午夜就写“凌晨0点”，并补上今天或明天。")
	}

	switch period {
	case "凌晨":
		if hour == 12 {
			hour = 0
		}
	case "中午":
		if hour < 11 {
			hour += 12
		}
	case "下午":
		if hour < 12 {
			hour += 12
		}
	case "傍晚", "晚上", "夜里":
		if hour < 12 {
			hour += 12
		} else if hour == 12 {
			hour = 0
		}
	}
	return hour, minute, dateToken, nil
}

func reminderSubmatch(text string, m []int, group int) string {
	idx := group * 2
	if idx+1 >= len(m) || m[idx] < 0 {
		return ""
	}
	return text[m[idx]:m[idx+1]]
}

func reminderClockTime(now time.Time, hour, minute int, dateToken string) (time.Time, error) {
	base := time.Date(now.Year(), now.Month(), now.Day(), hour, minute, 0, 0, now.Location())
	switch dateToken {
	case "今天":
		if !base.After(now) {
			return time.Time{}, fmt.Errorf("今天这个时间已经过了。换成“明天”或“N 分钟后”试试。")
		}
		return base, nil
	case "明天":
		return base.AddDate(0, 0, 1), nil
	case "后天":
		return base.AddDate(0, 0, 2), nil
	case "大后天":
		return base.AddDate(0, 0, 3), nil
	}
	if !base.After(now) {
		base = base.AddDate(0, 0, 1)
	}
	return base, nil
}

func reminderRelativeTime(text string, m []int, half bool, now time.Time) (time.Time, error) {
	valueToken := reminderSubmatch(text, m, 1)
	unit := reminderSubmatch(text, m, 2)
	value := 0
	if valueToken != "" {
		var ok bool
		value, ok = parseReminderNumber(valueToken)
		if !ok || value <= 0 || value > 10000 {
			return time.Time{}, fmt.Errorf("“%s”这个数量没看懂。", valueToken)
		}
	} else if !half {
		return time.Time{}, fmt.Errorf("没看懂提醒时间。")
	}

	var unitDuration time.Duration
	switch unit {
	case "秒钟", "秒":
		unitDuration = time.Second
	case "分钟", "分":
		unitDuration = time.Minute
	case "小时", "钟头":
		unitDuration = time.Hour
	case "天", "日":
		unitDuration = 24 * time.Hour
	case "周", "星期":
		unitDuration = 7 * 24 * time.Hour
	default:
		return time.Time{}, fmt.Errorf("没看懂时间单位“%s”。", unit)
	}

	d := time.Duration(value) * unitDuration
	if half {
		d += unitDuration / 2
	}
	if d <= 0 || d > reminderMaxDuration {
		return time.Time{}, fmt.Errorf("提醒时间太远了，本鱼最多支持十年内。")
	}
	at := now.Add(d).Truncate(time.Second)
	if !at.After(now) {
		at = now.Truncate(time.Second).Add(time.Second)
	}
	return at, nil
}

func buildReminderRepeatSchedule(kind string, hour, minute int) (schedule, label string, err error) {
	switch {
	case kind == "daily":
		return fmt.Sprintf("%d %d * * *", minute, hour), fmt.Sprintf("每天 %02d:%02d", hour, minute), nil
	case kind == "workday":
		return fmt.Sprintf("%d %d * * 1-5", minute, hour), fmt.Sprintf("每个工作日 %02d:%02d", hour, minute), nil
	case strings.HasPrefix(kind, "weekly:"):
		dow, ok := reminderWeekday(kind[len("weekly:"):])
		if !ok {
			return "", "", fmt.Errorf("星期几没看懂，用“每周一”到“每周日”。")
		}
		return fmt.Sprintf("%d %d * * %d", minute, hour, dow), fmt.Sprintf("每周%s %02d:%02d", kind[len("weekly:"):], hour, minute), nil
	default:
		return "", "", fmt.Errorf("重复方式没看懂。")
	}
}

func reminderWeekday(raw string) (int, bool) {
	switch raw {
	case "一", "1":
		return 1, true
	case "二", "2":
		return 2, true
	case "三", "3":
		return 3, true
	case "四", "4":
		return 4, true
	case "五", "5":
		return 5, true
	case "六", "6":
		return 6, true
	case "日", "天", "7":
		return 0, true
	default:
		return 0, false
	}
}

func isSingleCNNumeral(raw string) bool {
	runes := []rune(raw)
	return len(runes) == 1 && strings.ContainsRune("零〇一二两三四五六七八九十", runes[0])
}

func parseReminderNumber(raw string) (int, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, false
	}
	allDigits := true
	for _, r := range raw {
		if r < '0' || r > '9' {
			allDigits = false
			break
		}
	}
	if allDigits {
		n := 0
		for _, r := range raw {
			n = n*10 + int(r-'0')
			if n > 100000 {
				return 0, false
			}
		}
		return n, true
	}

	digits := map[rune]int{
		'零': 0, '〇': 0, '一': 1, '二': 2, '两': 2, '三': 3, '四': 4,
		'五': 5, '六': 6, '七': 7, '八': 8, '九': 9,
	}
	if idx := strings.Index(raw, "十"); idx >= 0 {
		left, right := raw[:idx], raw[idx+len("十"):]
		tens := 1
		if left != "" {
			n, ok := digits[[]rune(left)[0]]
			if !ok || len([]rune(left)) != 1 {
				return 0, false
			}
			tens = n
		}
		ones := 0
		if right != "" {
			n, ok := digits[[]rune(right)[0]]
			if !ok || len([]rune(right)) != 1 {
				return 0, false
			}
			ones = n
		}
		return tens*10 + ones, true
	}
	n := 0
	for _, r := range raw {
		d, ok := digits[r]
		if !ok {
			return 0, false
		}
		n = n*10 + d
	}
	return n, true
}

func reminderContentHasAnotherTime(text string) bool {
	return reminderClockRe.MatchString(text) ||
		reminderRelativeHalfRe.MatchString(text) ||
		reminderRelativeRe.MatchString(text)
}

func reminderContent(text string, spans ...[2]int) string {
	sort.Slice(spans, func(i, j int) bool { return spans[i][0] > spans[j][0] })
	for _, span := range spans {
		if span[0] < 0 || span[1] > len(text) || span[0] >= span[1] {
			continue
		}
		text = text[:span[0]] + " " + text[span[1]:]
	}
	return cleanReminderContent(text)
}

func cleanReminderContent(text string) string {
	text = strings.TrimSpace(text)
	text = strings.Trim(text, " \t，,。.：:；;！!？?、")
	for {
		changed := false
		for _, prefix := range []string{"帮我", "麻烦", "请", "给我", "一下", "记得"} {
			if !strings.HasPrefix(text, prefix) {
				continue
			}
			rest := strings.TrimPrefix(text, prefix)
			if rest != "" && !reminderCleanBoundary(rest) {
				continue
			}
			text = strings.TrimSpace(rest)
			text = strings.Trim(text, " \t，,。.：:；;！!？?、")
			changed = true
		}
		if !changed {
			break
		}
	}
	for _, suffix := range []string{"吧", "呀", "啊", "哦", "哈"} {
		text = strings.TrimSuffix(text, suffix)
	}
	text = strings.TrimSpace(text)
	if utf8.RuneCountInString(text) > reminderMaxContentRunes {
		text = string([]rune(text)[:reminderMaxContentRunes])
	}
	return text
}

func reminderCleanBoundary(s string) bool {
	r, _ := utf8.DecodeRuneInString(s)
	return r == ' ' || r == '\t' || strings.ContainsRune("，,。.：:；;！!？?、", r)
}

func formatChattyReminder(text string) string {
	text = strings.Join(strings.Fields(strings.TrimSpace(text)), " ")
	text = strings.TrimRight(text, " \t\r\n。.!！?？;；")
	if text == "" {
		return "到点啦，别忘了。"
	}
	if strings.HasPrefix(text, "记得") || strings.HasPrefix(text, "别忘") ||
		strings.HasPrefix(text, "该") || strings.HasSuffix(text, "吧") ||
		strings.HasSuffix(text, "啦") || strings.HasSuffix(text, "呀") ||
		strings.HasSuffix(text, "啊") || strings.HasSuffix(text, "哦") {
		return "到点啦，" + text + "。"
	}
	if strings.HasSuffix(text, "了") {
		return "到点啦，该" + text + "。"
	}
	return "到点啦，记得" + text + "。"
}

func reminderChatPrompt(content string) string {
	return fmt.Sprintf("现在就是定时提醒时间。请像朋友聊天一样，直接对用户说一句自然、口语化的提醒，提醒内容：%s。只输出这一句话，不要用“提醒：”标题、列表或 Markdown，不要解释，也不要反问。", content)
}

func reminderTimeUsage() string {
	return "没听出提醒时间。可以这样说：\n\n提醒我明天上午 9 点交报告\n\n30 分钟后提醒我喝水\n\n每天下午 5 点提醒我写日报"
}

func reminderContentUsage() string {
	return "时间听出来了，但要提醒你做什么？例如：提醒我明天上午 9 点交报告。"
}

func reminderJobName(content string) string {
	name := strings.TrimSpace(content)
	runes := []rune(name)
	if len(runes) > 30 {
		name = string(runes[:30]) + "…"
	}
	if name == "" {
		name = "提醒"
	}
	return "提醒：" + name
}

func formatReminderWhen(t time.Time) string {
	now := time.Now()
	ty, tm, td := now.Date()
	y, m, d := t.Date()
	if y == ty && m == tm && d == td {
		return "今天 " + t.Format("15:04")
	}
	tomorrow := now.AddDate(0, 0, 1)
	ny, nm, nd := tomorrow.Date()
	if y == ny && m == nm && d == nd {
		return "明天 " + t.Format("15:04")
	}
	return t.Format("01/02 15:04")
}

func (h *Handler) handleReminderRequest(ctx context.Context, client *ilink.Client, msg ilink.WeixinMessage, text, clientID string) bool {
	if strings.HasPrefix(strings.TrimSpace(text), "/") {
		return false
	}
	req, ok := parseReminderRequest(text)
	if !ok {
		return false
	}
	if h.cronStore == nil {
		_ = SendTextReply(ctx, client, msg.FromUserID, "定时提醒没启用（cron 存储没加载）。", msg.ContextToken, clientID)
		return true
	}
	if req.Err != "" {
		_ = SendTextReply(ctx, client, msg.FromUserID, req.Err, msg.ContextToken, clientID)
		return true
	}

	job := CronJob{
		ID:       uuid.New().String()[:8],
		Name:     reminderJobName(req.Content),
		Enabled:  true,
		Schedule: req.Schedule,
		Message:  req.Content,
		UserID:   msg.FromUserID,
		Direct:   true,
		State: JobState{
			NextRunAt:   req.Next,
			DeleteAfter: strings.HasPrefix(req.Schedule, "at:"),
		},
	}
	if err := h.cronStore.Add(job); err != nil {
		log.Printf("[reminder] failed to add job for %s: %v", msg.FromUserID, err)
		_ = SendTextReply(ctx, client, msg.FromUserID, "提醒没存下来，稍后再试。", msg.ContextToken, clientID)
		return true
	}
	log.Printf("[reminder] added job %s for %s (%s)", job.ID, msg.FromUserID, req.Schedule)

	when := formatReminderWhen(req.Next)
	if req.RepeatLabel != "" {
		when = req.RepeatLabel + "，下次 " + when
	}
	reply := fmt.Sprintf("好，定好了。\n\n时间：%s\n\n内容：%s\n\n任务号：%s\n\n取消：/cron delete %s\n\n到点我会像聊天一样发给你，你可以直接接着回；没收到就随便发句话，我给你补上。",
		when, req.Content, job.ID, job.ID)
	if err := SendTextReply(ctx, client, msg.FromUserID, reply, msg.ContextToken, clientID); err != nil {
		log.Printf("[reminder] failed to send confirmation to %s: %v", msg.FromUserID, err)
	}
	return true
}
