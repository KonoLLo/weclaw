package messaging

import "testing"

func TestDetectSearchTable(t *testing.T) {
	for _, s := range []string{
		"搜一下 今天的天气",
		"/s 上海到北京的机票",
		"/搜 茅台股价",
		"帮我查一下明天会不会下雨",
		"搜索 最新的显卡价格",
		"search for golang release notes",
		"今天天气怎么样",
		"你好",
		"这句话里的图是什么意思",
		"帮我看看这个方案",
		"查查这个方案有没有问题",
		"画一只戴帽子的橘猫",
	} {
		q, ok := detectSearchRequest(s)
		t.Logf("%-26q ok=%-5v query=%q", s, ok, q)
	}
}
