package chat

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/internal/lineeditor"
)

func TestMarkdownConversation(t *testing.T) {
	body := "## Вынік\n\nЗвычайны **важны** тэкст і `go test`.\n\n- першы\n- другі\n\n3. праверка\n4. зборка\n\n> цытата\n\n```go\n\tprintln(\"**literal**\")\n\nnext()\n```\n\n[дакументацыя](https://example.org/docs)\n"
	for _, color := range []bool{false, true} {
		t.Run(fmt.Sprint(color), func(t *testing.T) {
			var out bytes.Buffer
			d := newDisplay(&out, func(string) string { return "" })
			d.ui, d.color = &terminalUI{width: 100, editor: lineeditor.NewTerminal(&bytes.Buffer{}, "you> ")}, color
			item := sessionstore.Item{Kind: sessionstore.ItemModelResponse, Data: sessionstore.ModelResponse{Response: llm.Response{Output: []llm.Item{{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleAssistant, Text: body}}}}}}
			if err := d.item(item, true); err != nil {
				t.Fatal(err)
			}
			output := out.String()
			plain := terminalStyle.ReplaceAllString(output, "")
			for _, want := range []string{"Assistant\n", "Вынік\n\n", "Звычайны важны тэкст", "• першы", "• другі", "3. праверка", "4. зборка", "│ цытата", "╭─ go", "    println(\"**literal**\")\n\nnext()\n", "╰─", "дакументацыя (https://example.org/docs)"} {
				if !strings.Contains(plain, want) {
					t.Errorf("missing %q:\n%s", want, plain)
				}
			}
			for _, unwanted := range []string{"assistant>", "## Вынік", "**важны**", "```", "\x1b[35m"} {
				if strings.Contains(output, unwanted) {
					t.Errorf("raw/noisy formatting %q:\n%s", unwanted, output)
				}
			}
			if color {
				if !strings.Contains(output, "\x1b[1mважны\x1b[0m") || !strings.Contains(output, "\x1b[36mgo test\x1b[0m") || strings.Contains(output, "\x1b[35m") {
					t.Fatal("missing selective emphasis or whole-body color")
				}
			} else if strings.ContainsRune(output, 27) {
				t.Fatal("ANSI with NO_COLOR")
			}
		})
	}
}

func TestMarkdownSafeTextAndFallback(t *testing.T) {
	var out bytes.Buffer
	d := newDisplay(&out, func(name string) string {
		if name == "OPENAI_API_KEY" {
			return "configured-private-key"
		}
		return ""
	})
	d.ui = &terminalUI{width: 70}
	body := "## &#27;[31mnot ANSI\n\n**configured-private-key** &#x202e;plain\\*text &amp; more\n\n[link](https://user:pass@example.org/?api_key=private) ![alt](image.png)\n\n<script>not executed</script>\n\n```sh\necho API_KEY=private\n```\n"
	if err := d.message("assistant", body); err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"configured-private-key", "user:pass", "api_key=private", "API_KEY=private", "\x1b", "\u202e"} {
		if strings.Contains(out.String(), forbidden) {
			t.Fatalf("unsafe text %q: %q", forbidden, out.String())
		}
	}
	for _, want := range []string{"plain*text & more", "[redacted]", "[image: alt] (image.png)", "<script>not executed</script>"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("lost literal text %q: %s", want, out.String())
		}
	}
	out.Reset()
	d.ui = nil
	body = "**raw Markdown**\n```go\nx()\n```"
	if err := d.message("assistant", body); err != nil {
		t.Fatal(err)
	}
	if out.String() != "assistant> "+body+"\n" {
		t.Fatalf("piped format changed: %q", out.String())
	}
}

func TestMarkdownWrapAndSourcePreservation(t *testing.T) {
	var out bytes.Buffer
	d := newDisplay(&out, func(string) string { return "" })
	d.ui, d.color = &terminalUI{width: 24}, true
	body := "**Адзін два тры чатыры пяць шэсць сем восем дзевяць**.\n\n```\n  abcdefghijklmnopqrstuvwxyz\n```"
	got := d.markdown(body)
	plain := terminalStyle.ReplaceAllString(got, "")
	if !strings.Contains(plain, "\n  abcdefghijklmnopqrstuvwxyz\n") {
		t.Fatal("code was reflowed or truncated")
	}
	prose := strings.Split(plain, "╭─")[0]
	for _, line := range strings.Split(prose, "\n") {
		if utf8.RuneCountInString(line) > 23 {
			t.Fatalf("unwrapped prose: %q", line)
		}
	}
	if !strings.Contains(strings.Join(strings.Fields(prose), " "), "Адзін два тры чатыры пяць шэсць сем восем дзевяць.") {
		t.Fatalf("words lost or reordered: %q", plain)
	}
	if !utf8.ValidString(got) {
		t.Fatal("invalid UTF-8")
	}
}

func TestCompactCommandSummary(t *testing.T) {
	for _, tt := range []struct{ input, want string }{
		{"Bash: go test ./...", "Bash: go test ./..."},
		{"Bash: python3 - <<'PY'\nprint('body hidden')\nPY\n", "Bash: python3 - <<'PY' · +2 lines"},
		{"ViewImage: /tmp/image.png", "ViewImage: /tmp/image.png"},
	} {
		if got := commandSummary(tt.input, 80); got != tt.want {
			t.Errorf("summary=%q, want %q", got, tt.want)
		}
	}
	for _, width := range []int{1, 8, 20, 40} {
		got := commandSummary("Bash: "+strings.Repeat("беларуская ", 40)+"\nmore\nlines", width)
		if !utf8.ValidString(got) || utf8.RuneCountInString(got) > width {
			t.Fatalf("invalid/unbounded summary: %q", got)
		}
	}
}

func FuzzMarkdownTerminalSafety(f *testing.F) {
	for _, value := range []string{"**hello** `code`", "&#27;[2J", "> - [link](https://example.org)", "```go\n\tprintln(1)\n```", "Беларуская мова"} {
		f.Add(value)
	}
	f.Fuzz(func(t *testing.T, body string) {
		if len(body) > 4096 {
			t.Skip()
		}
		d := newDisplay(&bytes.Buffer{}, func(string) string { return "" })
		d.ui, d.color = &terminalUI{width: 40}, true
		output := terminalStyle.ReplaceAllString(d.markdown(body), "")
		if strings.ContainsRune(output, 27) || !utf8.ValidString(output) {
			t.Fatalf("unsafe output: %q", output)
		}
	})
}

func TestMarkdownCopyHasNoDecorativeIndentation(t *testing.T) {
	for _, color := range []bool{false, true} {
		t.Run(fmt.Sprint(color), func(t *testing.T) {
			var out bytes.Buffer
			d := newDisplay(&out, func(string) string { return "" })
			d.ui, d.color = &terminalUI{width: 32}, color
			if err := d.message("assistant", "## Загаловак\n\n"+strings.Repeat("тэкст для капіравання ", 8)); err != nil {
				t.Fatal(err)
			}
			plain := terminalStyle.ReplaceAllString(out.String(), "")
			for _, line := range strings.Split(plain, "\n") {
				if strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") {
					t.Fatalf("decorative whitespace in copied prose/header: %q", line)
				}
			}
			code := "if ready:\n    first()\n\n    second()\n"
			for _, body := range []string{
				"```py\n" + code + "```",
				"- Example:\n\n  ```py\n  if ready:\n      first()\n\n      second()\n  ```",
				"> ```py\n> if ready:\n>     first()\n>\n>     second()\n> ```",
			} {
				plain := terminalStyle.ReplaceAllString(d.markdown(body), "")
				_, after, ok := strings.Cut(plain, "╭─ py\n")
				if !ok {
					t.Fatal("missing code heading")
				}
				var copied strings.Builder
				for _, line := range strings.Split(after, "\n") {
					if strings.Contains(line, "╰─") {
						break
					}
					copied.WriteString(line + "\n")
				}
				if copied.String() != code {
					t.Fatalf("code selection changed indentation/content: got %q, want %q", copied.String(), code)
				}
			}
		})
	}
}
