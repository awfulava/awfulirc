package awfulirc

import (
	"strings"

	"golang.org/x/net/html"
)

var (
	quoteReplacer = strings.NewReplacer(
		"‘", "'",
		"’", "'",
		"“", "\"",
		"”", "\"",
	)
)

func extractTextFromChild(node *html.Node) string {
	for n := range node.ChildNodes() {
		if n.Type == html.TextNode {
			return strings.TrimSpace(n.Data)
		}
	}
	return ""
}
