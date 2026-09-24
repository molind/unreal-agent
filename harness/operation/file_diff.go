package operation

import (
	"fmt"
	"strconv"
	"strings"
)

// One valid contiguous unified hunk. Deliberately bounded in the UI, while the
// complete patch is captured on disk. It need not be a minimal edit script.
func fileDiff(path, before, after string, created bool) string {
	split := func(s string) []string {
		if s == "" {
			return nil
		}
		lines := strings.SplitAfter(s, "\n")
		if lines[len(lines)-1] == "" {
			lines = lines[:len(lines)-1]
		}
		return lines
	}
	a, b := split(before), split(after)
	prefix := 0
	for prefix < len(a) && prefix < len(b) && a[prefix] == b[prefix] {
		prefix++
	}
	suffix := 0
	for suffix < len(a)-prefix && suffix < len(b)-prefix && a[len(a)-1-suffix] == b[len(b)-1-suffix] {
		suffix++
	}
	start := max(0, prefix-3)
	endA, endB := min(len(a), len(a)-suffix+3), min(len(b), len(b)-suffix+3)
	name := path
	if strings.ContainsAny(name, "\n\r\t\"\\") {
		name = strconv.Quote(name)
	}
	oldName := name
	if created {
		oldName = "/dev/null"
	}
	var out strings.Builder
	fmt.Fprintf(&out, "--- %s\n+++ %s\n", oldName, name)
	oldStart, newStart := start+1, start+1
	if endA == start {
		oldStart = start
	}
	if endB == start {
		newStart = start
	}
	fmt.Fprintf(&out, "@@ -%d,%d +%d,%d @@\n", oldStart, endA-start, newStart, endB-start)
	line := func(sign byte, text string) {
		out.WriteByte(sign)
		out.WriteString(text)
		if !strings.HasSuffix(text, "\n") {
			out.WriteString("\n\\ No newline at end of file\n")
		}
	}
	for _, s := range a[start:prefix] {
		line(' ', s)
	}
	for _, s := range a[prefix : len(a)-suffix] {
		line('-', s)
	}
	for _, s := range b[prefix : len(b)-suffix] {
		line('+', s)
	}
	for _, s := range a[len(a)-suffix : endA] {
		line(' ', s)
	}
	return out.String()
}
