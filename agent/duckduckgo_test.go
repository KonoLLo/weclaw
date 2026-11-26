package agent

import (
	"strings"
	"testing"
)

const ddgFixture = `<html><body><div id="links" class="results">
<div class="result results_links results_links_deep web-result">
<h2 class="result__title"><a rel="nofollow" class="result__a" href="https://a.example/1">标题一</a></h2>
<a class="result__snippet" href="https://a.example/1">摘要一，含 <b>关键词</b>。</a>
</div>
<div class="result results_links web-result">
<h2><a class="result__a" href="https://b.example/2">标题二</a></h2>
<div class="result__snippet">摘要二</div>
</div>
</div></body></html>`

func TestParseDuckDuckGo(t *testing.T) {
	res, err := parseDuckDuckGo(strings.NewReader(ddgFixture), 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 2 {
		t.Fatalf("got %d results: %+v", len(res), res)
	}
	if res[0].Title != "标题一" || !strings.Contains(res[0].Content, "摘要一") || !strings.Contains(res[0].Content, "关键词") {
		t.Fatalf("first result: %+v", res[0])
	}
	if res[1].URL != "https://b.example/2" || res[1].Title != "标题二" {
		t.Fatalf("second result: %+v", res[1])
	}
}
