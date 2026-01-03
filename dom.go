package awfulirc

import (
	"strings"

	"golang.org/x/net/html"
)

func extractTextFromChild(node *html.Node) string {
	for n := range node.ChildNodes() {
		if n.Type == html.TextNode {
			return strings.TrimSpace(n.Data)
		}
	}
	return ""
}

func extractTextFromDeepChild(node *html.Node) string {
	n := node
	for n.Type != html.TextNode {
		for child := range n.ChildNodes() {
			n = child
			break
		}
	}
	if n.Type == html.TextNode {
		return strings.TrimSpace(n.Data)
	}
	return ""
}
