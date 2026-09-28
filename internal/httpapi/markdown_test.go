package httpapi

import (
	"strings"
	"testing"
)

func TestRenderMarkdownFormatsAgentTextAndRejectsUnsafeHTML(t *testing.T) {
	input := "# Result\n\n**Done** with [details](https://example.com).\n\n- First\n- Second\n\n```go\nfmt.Println(1)\n```\n\n| Key | Value |\n| --- | --- |\n| one | two |\n\n[bad](javascript:alert(1)) [local](/Users/example/file.jsonl)\n<script>alert(1)</script>\n![remote](https://example.com/image.png)"
	html, err := renderMarkdown(input)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"<h1>Result</h1>", "<strong>Done</strong>", `<a href="https://example.com"`, "details</a>", "<li>First</li>", "<pre><code>", "<table>"} {
		if !strings.Contains(html, want) {
			t.Errorf("rendered Markdown does not contain %q: %s", want, html)
		}
	}
	for _, unsafe := range []string{"javascript:", `href="/Users`, "<script", "<img"} {
		if strings.Contains(html, unsafe) {
			t.Errorf("rendered Markdown contains %q: %s", unsafe, html)
		}
	}
}
