package messaging

import "testing"

func TestDetectImageTable(t *testing.T) {
	for _, s := range []string{
		"画一只戴帽子的橘猫",
		"帮我画一张雪景",
		"给我生图：赛博朋克夜市",
		"出图",
		"/图 一只戴帽子的橘猫",
		"/图",
		"draw me a cat",
		"generate an image of a fox",
		"今天天气怎么样",
		"你好",
		"这句话里的图是什么意思",
		"我刚才让你画的那只猫挺好看",
		"帮我看看这个方案",
		"模型有哪些",
		"这个视频里的人物是谁",
		"给我生图：赛博朋克夜市",
		"生一张海报",
		"draw me a cat",
		"please draw a fox in the snow",
	} {
		p, ok := detectImageRequest(s)
		t.Logf("%-24q ok=%-5v prompt=%q", s, ok, p)
	}
}
