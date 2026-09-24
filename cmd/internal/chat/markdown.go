package chat

import (
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

// Parse CommonMark with a maintained parser, but render only terminal text.
// No HTML renderer, OSC hyperlinks, external resources, or model-supplied ANSI
// reach the terminal. Sanitize again after decoding Markdown entities/escapes.
type markdownView struct {
	source []byte
	width  int
	color  bool
	safe   func(string) string
}

func (d *display) markdown(body string) string {
	v := markdownView{source: []byte(d.safe(body)), width: min(100, d.columns()-1), color: d.color, safe: d.safe}
	root := goldmark.DefaultParser().Parse(text.NewReader(v.source))
	return strings.TrimRight(v.blocks(root, ""), "\n") + "\n"
}

func paint(color bool, style, value string) string {
	if !color || style == "" || value == "" {
		return value
	}
	return "\x1b[" + style + "m" + value + "\x1b[0m"
}

var terminalStyle = regexp.MustCompile(`\x1b\[[0-9;]*m`)

func visibleLength(value string) int {
	return utf8.RuneCountInString(terminalStyle.ReplaceAllString(value, ""))
}

// Wrap prose at words rather than clipping it. Long code/URL tokens remain
// intact and may wrap naturally in the terminal. Only our generated SGR has
// zero width; callers must sanitize external text before adding styles.
func wrapProse(value, prefix string, width int) string {
	var out strings.Builder
	for _, line := range strings.Split(value, "\n") {
		out.WriteString(prefix)
		column, used := visibleLength(prefix), false
		for _, word := range strings.Fields(line) {
			n := visibleLength(word)
			if n == 0 {
				out.WriteString(word)
				continue
			}
			if used && column+1+n > width {
				out.WriteByte('\n')
				out.WriteString(prefix)
				column, used = visibleLength(prefix), false
			}
			if used {
				out.WriteByte(' ')
				column++
			}
			out.WriteString(word)
			column += n
			used = true
		}
		out.WriteByte('\n')
	}
	return out.String()
}

func (v markdownView) decoded(value []byte) string {
	value = util.UnescapePunctuations(value)
	value = util.ResolveNumericReferences(value)
	return v.safe(string(util.ResolveEntityNames(value)))
}

func (v markdownView) inline(parent ast.Node, style string) string {
	var out strings.Builder
	for child := parent.FirstChild(); child != nil; child = child.NextSibling() {
		switch n := child.(type) {
		case *ast.Text:
			value := v.safe(string(n.Value(v.source)))
			if !n.IsRaw() {
				value = v.decoded(n.Value(v.source))
			}
			out.WriteString(paint(v.color, style, value))
			if n.HardLineBreak() {
				out.WriteByte('\n')
			} else if n.SoftLineBreak() {
				out.WriteByte(' ')
			}
		case *ast.String:
			out.WriteString(paint(v.color, style, v.safe(string(n.Value))))
		case *ast.Emphasis:
			code := "3"
			if n.Level == 2 {
				code = "1"
			}
			out.WriteString(v.inline(n, strings.Trim(style+";"+code, ";")))
		case *ast.CodeSpan:
			var code strings.Builder
			for c := n.FirstChild(); c != nil; c = c.NextSibling() {
				code.WriteString(strings.ReplaceAll(string(c.(*ast.Text).Value(v.source)), "\n", " "))
			}
			value := v.safe(code.String())
			if !v.color {
				value = "`" + value + "`"
			}
			out.WriteString(paint(v.color, "36", value))
		case *ast.Link:
			label, destination := v.inline(n, "4;36"), v.decoded(n.Destination)
			out.WriteString(label)
			if terminalStyle.ReplaceAllString(label, "") != destination {
				out.WriteString(" (" + paint(v.color, "4", destination) + ")")
			}
		case *ast.AutoLink:
			out.WriteString(paint(v.color, "4;36", v.safe(string(n.URL(v.source)))))
		case *ast.Image:
			out.WriteString("[image: " + v.inline(n, style) + "] (" + v.decoded(n.Destination) + ")")
		case *ast.RawHTML:
			out.WriteString(v.safe(string(n.Segments.Value(v.source))))
		default:
			out.WriteString(v.inline(n, style))
		}
	}
	return out.String()
}

func (v markdownView) blocks(parent ast.Node, prefix string) string {
	var out strings.Builder
	for n := parent.FirstChild(); n != nil; n = n.NextSibling() {
		switch node := n.(type) {
		case *ast.Heading:
			out.WriteString(wrapProse(v.inline(n, "1"), prefix, v.width) + "\n")
		case *ast.Paragraph:
			out.WriteString(wrapProse(v.inline(n, ""), prefix, v.width) + "\n")
		case *ast.TextBlock:
			out.WriteString(wrapProse(v.inline(n, ""), prefix, v.width))
		case *ast.List:
			index := node.Start
			for item := n.FirstChild(); item != nil; item = item.NextSibling() {
				marker := "• "
				if node.IsOrdered() {
					marker = fmt.Sprintf("%d. ", index)
					index++
				}
				indent := prefix + strings.Repeat(" ", utf8.RuneCountInString(marker))
				body := v.blocks(item, indent)
				out.WriteString(prefix + marker + strings.TrimPrefix(body, indent))
			}
			out.WriteByte('\n')
		case *ast.Blockquote:
			out.WriteString(v.blocks(n, prefix+"│ "))
		case *ast.FencedCodeBlock:
			out.WriteString(v.code(node.Lines().Value(v.source), v.safe(string(node.Language(v.source))), prefix))
		case *ast.CodeBlock:
			out.WriteString(v.code(node.Lines().Value(v.source), "code", prefix))
		case *ast.ThematicBreak:
			out.WriteString(prefix + paint(v.color, "2", strings.Repeat("─", max(1, min(32, v.width-visibleLength(prefix))))) + "\n\n")
		case *ast.HTMLBlock:
			value := string(node.Lines().Value(v.source))
			if node.HasClosure() {
				value += string(node.ClosureLine.Value(v.source))
			}
			out.WriteString(v.code([]byte(value), "html", prefix))
		default:
			out.WriteString(v.blocks(n, prefix))
		}
	}
	return out.String()
}

func (v markdownView) code(source []byte, language, prefix string) string {
	// Use ordinary Markdown fences; lengthen them if the code contains a
	// fence itself so the displayed block remains safe to copy as Markdown.
	marker := byte('`')
	if strings.ContainsRune(language, '`') {
		marker = '~'
	}
	length, run := 3, 0
	for _, b := range source {
		if b == marker {
			run++
			length = max(length, run+1)
		} else {
			run = 0
		}
	}
	fence := strings.Repeat(string(marker), length)
	opening := fence + language
	if marker == '~' && language != "" {
		opening = fence + " " + language
	}
	var out strings.Builder
	out.WriteString(prefix + paint(v.color, "2", opening) + "\n")
	var diff diffHighlighter
	isDiff := strings.EqualFold(language, "diff") || strings.EqualFold(language, "patch")
	for _, line := range strings.Split(strings.TrimSuffix(v.safe(string(source)), "\n"), "\n") {
		// Code rows carry only source indentation, never a decorative gutter
		// or the parent list/quote prefix. Keep nesting on the heading/footer
		// so selecting the code itself does not copy UI characters. Tabs are
		// expanded only in the view; canonical history is never modified.
		style := ""
		if isDiff {
			style = diff.lineStyle(line)
		}
		// Reset before the newline: neither the next row nor the live prompt
		// should inherit a diff background. Do not pad/reflow copied text.
		out.WriteString(paint(v.color, style, strings.ReplaceAll(line, "\t", "    ")) + "\n")
	}
	out.WriteString(prefix + paint(v.color, "2", fence) + "\n\n")
	return out.String()
}
