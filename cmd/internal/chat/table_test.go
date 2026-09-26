package chat

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

const guruTable = `| Падзея | Калі фіксуем | Навошта |
|---|---|---|
| search_started | Запусцілі першы запыт спробы | Ведаць колькасць спроб, уключаючы тыя, што скончыліся памылкай |
| results_shown | Актуальная выдача сапраўды трапіла на экран | Адрозніваць атрыманы адказ ад паказанага |
| result_selected | Карыстальнік выбраў канкрэтнае месца са спіса або з вынікаў на карце | Першы сігнал «знайшоў» |
| place_saved | Месца паспяхова захавана | Мацнейшы сігнал |
| navigation_started | Навігацыя сапраўды пачалася да гэтага месца | Мацнейшы сігнал |
| search_closed | Карыстальнік яўна выйшаў з пошуку | Адрозніваць закрытую спробу без дзеяння ад незавершанай |`

func renderTableTest(body string, width int, color bool) string {
	d := newDisplay(&bytes.Buffer{}, func(string) string { return "" })
	d.ui, d.color = &terminalUI{width: width}, color
	return d.markdown(body)
}

func TestMarkdownGuruTable(t *testing.T) {
	for _, width := range []int{60, 80, 100, 160} {
		for _, color := range []bool{false, true} {
			t.Run(fmt.Sprintf("%d/%t", width, color), func(t *testing.T) {
				got := renderTableTest("2. Што збіраем у Guru\n\n"+guruTable+"\n\nНе трэба падзеі на кожны націск клавішы.", width, color)
				plain := terminalStyle.ReplaceAllString(got, "")
				if !strings.Contains(plain, "┌") || strings.Contains(plain, "|---|") || !strings.Contains(plain, "Не трэба падзеі") {
					t.Fatalf("table not rendered or following prose lost:\n%s", got)
				}
				// Reassemble each column of each logical row. Every word must be
				// preserved in its original cell, not just somewhere in the output.
				var rows [][]string
				var cells []string
				for _, line := range strings.Split(plain, "\n") {
					if visibleLength(line) > min(100, width-1) {
						t.Fatalf("line exceeds viewport: %q", line)
					}
					if strings.HasPrefix(line, "│") {
						if cells == nil {
							cells = make([]string, 3)
						}
						parts := strings.Split(line, "│")
						if len(parts) != 5 {
							t.Fatalf("broken grid: %q", line)
						}
						for i := range cells {
							cells[i] += " " + strings.TrimSpace(parts[i+1])
						}
					} else if cells != nil {
						rows = append(rows, cells)
						cells = nil
					}
				}
				if len(rows) != 7 {
					t.Fatalf("got %d rows:\n%s", len(rows), plain)
				}
				source := strings.Split(guruTable, "\n")
				for r, row := range rows {
					index := r
					if r > 0 {
						index++ // delimiter is not a data row
					}
					want := strings.Split(source[index], "|")[1:4]
					for i, cell := range row {
						if strings.Join(strings.Fields(cell), " ") != strings.TrimSpace(want[i]) {
							t.Fatalf("row %d column %d lost text: %q, want %q", r, i, cell, want[i])
						}
					}
				}
				if !utf8.ValidString(got) || (!color && strings.ContainsRune(got, 27)) {
					t.Fatal("invalid UTF-8 or ANSI with NO_COLOR")
				}
			})
		}
	}
}

func TestMarkdownTableAlignmentAndUnicodeWidths(t *testing.T) {
	body := "| Left | Right | Center |\n|:---|---:|:---:|\n| 世界 | 12 | e\u0301 |\n| x | 3 | 🐈 |"
	want := "┌──────┬───────┬────────┐\n" +
		"│ Left │ Right │ Center │\n" +
		"├──────┼───────┼────────┤\n" +
		"│ 世界 │    12 │   e\u0301    │\n" +
		"├──────┼───────┼────────┤\n" +
		"│ x    │     3 │   🐈   │\n" +
		"└──────┴───────┴────────┘\n"
	for _, color := range []bool{false, true} {
		got := terminalStyle.ReplaceAllString(renderTableTest(body, 80, color), "")
		if got != want {
			t.Fatalf("alignment:\n%s\nwant:\n%s", got, want)
		}
	}
}

func TestMarkdownTableSyntaxNestingAndFallback(t *testing.T) {
	for _, tt := range []struct {
		name, body string
		width      int
		want       []string
		grid       bool
	}{
		{"optional pipes", "Name | Value\n--- | ---\none | two", 80, []string{"│ one  │ two   │"}, true},
		{"escaped pipes and inline", "| A | B |\n|---|---|\n| **bold** a\\|b | `x\\|y` &amp; [doc](https://e.org) |", 80, []string{"bold a|b", "`x|y` & doc (https://e.org)"}, true},
		{"short rows", "| A | B |\n|---|---|\n| one |", 80, []string{"│ one │   │"}, true},
		{"header only", "| A | B |\n|---|---|", 80, []string{"│ A │ B │"}, true},
		{"quote", "> | A | B |\n> |---|---|\n> | one | two |", 24, []string{"│ ┌", "│ │ one │ two │"}, true},
		{"list", "- Table:\n\n  | A | B |\n  |---|---|\n  | one | two |", 24, []string{"• Table:", "  ┌", "  │ one │ two │"}, true},
		{"narrow", "| Event | Why |\n|---|---|\n| search_started | Count attempts |\n| search_closed | Explicit exit |", 24, []string{"Event: search_started", "Why: Count attempts\n\nEvent: search_closed", "Why: Explicit exit"}, false},
		{"empty label", "| | Why |\n|---|---|\n| search_started | Count attempts |", 20, []string{"Column 1:", "search_started", "Why: Count attempts"}, false},
		{"not a table", "a | b\nnot a delimiter", 80, []string{"a | b not a delimiter"}, false},
		{"code", "```md\n| A | B |\n|---|---|\n| one | two |\n```", 80, []string{"| A | B |\n|---|---|\n| one | two |"}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := renderTableTest(tt.body, tt.width, false)
			for _, want := range tt.want {
				if !strings.Contains(got, want) {
					t.Errorf("missing %q:\n%s", want, got)
				}
			}
			if strings.Contains(got, "┌") != tt.grid {
				t.Fatalf("wrong layout:\n%s", got)
			}
		})
	}
	for width := 1; width <= 40; width++ {
		if got := renderTableTest(guruTable, width, true); !utf8.ValidString(got) || !strings.Contains(got, "search_closed") {
			t.Fatalf("narrow viewport lost data at %d: %q", width, got)
		}
	}
}

func TestTableCellStylesDoNotBleed(t *testing.T) {
	for _, style := range []string{"1", "3", "4;36", "36"} {
		lines := tableCellLines(paint(true, style, "one two three four"), 9)
		want := []string{paint(true, style, "one two"), paint(true, style, "three"), paint(true, style, "four")}
		if strings.Join(lines, "\n") != strings.Join(want, "\n") {
			t.Fatalf("unbalanced wrapped style: %q", lines)
		}
	}
	got := renderTableTest("| Header | Other |\n|---|---|\n| **one two three four five six** | end |", 28, true)
	if strings.Contains(got, "\x1b[1mone two three four five six") {
		t.Fatal("cell did not wrap")
	}
	for _, line := range strings.Split(got, "\n") {
		if strings.Contains(line, "│") && !strings.HasSuffix(line, "\x1b[0m") {
			t.Fatalf("style escaped table row: %q", line)
		}
	}
}

func TestMarkdownTableSafetyAndPipedOutput(t *testing.T) {
	body := "| Header | Value |\n|---|---|\n| &#27;[2J | &#x202e;safe &amp; more |\n| API_KEY=private | [link](https://user:pass@example.org/) |"
	got := terminalStyle.ReplaceAllString(renderTableTest(body, 100, true), "")
	for _, forbidden := range []string{"\x1b", "\u202e", "private", "user:pass"} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("unsafe table %q: %q", forbidden, got)
		}
	}
	if !strings.Contains(got, "safe & more") || !strings.Contains(got, "[redacted]") {
		t.Fatalf("lost safe text: %s", got)
	}
	var out bytes.Buffer
	d := newDisplay(&out, func(string) string { return "" })
	if err := d.message("assistant", guruTable); err != nil {
		t.Fatal(err)
	}
	if out.String() != "assistant> "+guruTable+"\n" {
		t.Fatal("piped Markdown changed")
	}
}
