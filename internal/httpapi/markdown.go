package httpapi

import (
	"bytes"
	"sync"

	"github.com/microcosm-cc/bluemonday"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
)

var markdownPool = sync.Pool{New: func() any {
	return goldmark.New(goldmark.WithExtensions(extension.GFM))
}}

var markdownPolicy = func() *bluemonday.Policy {
	policy := bluemonday.NewPolicy()
	policy.AllowElements("p", "br", "strong", "em", "del", "ul", "ol", "li", "blockquote", "pre", "code", "hr",
		"h1", "h2", "h3", "h4", "h5", "h6", "table", "thead", "tbody", "tr", "th", "td", "a")
	policy.AllowStandardURLs()
	policy.AllowRelativeURLs(false)
	policy.AllowAttrs("href").OnElements("a")
	return policy
}()

func renderMarkdown(text string) (string, error) {
	markdown := markdownPool.Get().(goldmark.Markdown)
	defer markdownPool.Put(markdown)
	var output bytes.Buffer
	if err := markdown.Convert([]byte(text), &output); err != nil {
		return "", err
	}
	return markdownPolicy.Sanitize(output.String()), nil
}
