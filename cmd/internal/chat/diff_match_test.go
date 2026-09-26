package chat

import (
	"slices"
	"strings"
	"testing"
)

func TestDiffScreenshotRegression(t *testing.T) {
	header := "func (d *display) markdown(body string) string {"
	view := "    v := markdownView{source: []byte(d.safe(body)), width: min(100, d.columns()-1), color: d.color, safe: d.safe}"
	inserted := "var terminalMarkdown = goldmark.New(goldmark.WithExtensions(extension.Table))"
	lines := []string{
		"-" + header, "-" + view,
		"-    root := goldmark.DefaultParser().Parse(text.NewReader(v.source))",
		"+" + inserted, "+", "+" + header, "+" + view,
		"+    root := terminalMarkdown.Parser().Parse(text.NewReader(v.source))",
	}
	highlightDiff(lines)
	want := []string{
		diffTestRow("-" + header), diffTestRow("-" + view),
		diffTestRow("-    root := ", "goldmark", ".", "DefaultParser", "().Parse(text.NewReader(v.source))"),
		diffTestRow("+" + inserted), diffTestRow("+"), diffTestRow("+" + header), diffTestRow("+" + view),
		diffTestRow("+    root := ", "terminalMarkdown", ".", "Parser", "().Parse(text.NewReader(v.source))"),
	}
	if !slices.Equal(lines, want) {
		t.Fatalf("inserted declaration disrupted matching:\n%q\nwant:\n%q", lines, want)
	}
}

func TestDiffWholeTokensAndUnpairedRows(t *testing.T) {
	for _, tt := range []struct {
		name, before, after string
		want                []string
	}{
		{"identifiers", "return utf8.RuneCountInString(value)", "return uniseg.StringWidth(value)", []string{
			diffTestRow("-return ", "utf8", ".", "RuneCountInString", "(value)"),
			diffTestRow("+return ", "uniseg", ".", "StringWidth", "(value)"),
		}},
		{"numeric tokens", "timeout = 12345;", "timeout = 12346;", []string{
			diffTestRow("-timeout = ", "12345", ";"), diffTestRow("+timeout = ", "12346", ";"),
		}},
		{"combining marks", "word = cafe\u0301", "word = cafe\u0300", []string{
			diffTestRow("-word = ", "cafe\u0301"), diffTestRow("+word = ", "cafe\u0300"),
		}},
		{"emoji graphemes", "reaction = 👍🏽", "reaction = 👍🏻", []string{
			diffTestRow("-reaction = ", "👍🏽"), diffTestRow("+reaction = ", "👍🏻"),
		}},
		{"indentation is not a match", "    old_word", "    new_word", []string{
			diffTestRow("-    old_word"), diffTestRow("+    new_word"),
		}},
		{"coincidental letters", "hello", "world", []string{
			diffTestRow("-hello"), diffTestRow("+world"),
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			lines := []string{"-" + tt.before, "+" + tt.after}
			highlightDiff(lines)
			if !slices.Equal(lines, tt.want) {
				t.Fatalf("token highlight: %q, want %q", lines, tt.want)
			}
		})
	}
}

func TestDiffApprovedPaletteAndBoldReset(t *testing.T) {
	lines := []string{"-timeout = 30;", "+timeout = 60;", " neutral"}
	highlightDiff(lines)
	want := []string{
		"\x1b[0;38;5;224;48;5;52m-timeout = \x1b[1;38;5;231;48;5;124m30\x1b[0;38;5;224;48;5;52m;\x1b[0m",
		"\x1b[0;38;5;194;48;5;22m+timeout = \x1b[1;38;5;231;48;5;34m60\x1b[0;38;5;194;48;5;22m;\x1b[0m",
		" neutral",
	}
	if !slices.Equal(lines, want) {
		t.Fatalf("palette/bold reset: %q", lines)
	}
}

func TestDiffMatchingBudgets(t *testing.T) {
	// Too many rows for the alignment matrix: keep full, uniform rows.
	lines := strings.Split(strings.Repeat("-old()\n", 1500)+strings.Repeat("+new()\n", 1500), "\n")
	highlightDiff(lines)
	for i, line := range lines[:3000] {
		want := diffTestRow("-old()")
		if i >= 1500 {
			want = diffTestRow("+new()")
		}
		if line != want {
			t.Fatal("oversized block must keep uniform rows")
		}
	}
	// Few rows, but too much content to repeatedly compare across all pairs.
	row := newDiffRow(0, strings.Repeat("word ", 10000))
	budget := diffMaxCells
	if pairs := diffRowPairs(slices.Repeat([]diffRow{row}, 8), slices.Repeat([]diffRow{row}, 8), &budget); len(pairs) != 0 {
		t.Fatal("row-content work was not bounded")
	}
	// Token LCS must share the remaining budget and retain whole common edges
	// when the middle is too expensive. It must never split common letters.
	before, after := diffTokens("prefix "+strings.Repeat("old ", 2000)+"suffix"), diffTokens("prefix "+strings.Repeat("new ", 2000)+"suffix")
	budget = 32
	removed, added := diffChangedTokens(before, after, &budget)
	if budget < 0 || removed[0] || added[0] || removed[len(removed)-1] || added[len(added)-1] || !removed[2] || !added[2] {
		t.Fatal("token fallback lost common edges or exceeded budget")
	}
}
