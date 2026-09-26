package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestTTYTablesAndDiffPalette(t *testing.T) {
	for _, noColor := range []bool{false, true} {
		t.Run(fmt.Sprint(noColor), func(t *testing.T) {
			table := "| Event | When | Why |\n|---|---|---|\n| search_started | First query | Count attempts |\n| search_closed | Exit | Closed attempt |"
			patch := "-timeout = 30;\n+timeout = 60;\n+new_row()"
			markdown := table + "\n\n```diff\n" + patch + "\n```\n\nRENDER DONE."
			requests := make(chan string, 4)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				requests <- string(body)
				writeResponse(w, []any{messageOutput(markdown)})
			}))
			defer server.Close()
			env := []string{"TERM=xterm"}
			if noColor {
				env = append(env, "NO_COLOR=1")
			}
			cli := startTTY(t, env, "-provider", "openai", "-base-url", server.URL, t.TempDir())
			cli.wait("you> ")
			s := &pickerScreen{t, cli, newLiveScreen(40, 24)}
			s.resizeTo(80, 32)
			cli.send("render table and diff\r")
			select {
			case <-requests:
			case <-time.After(8 * time.Second):
				t.Fatal("no model request")
			}
			cli.wait("RENDER DONE.")
			s.waitPrompt()
			for _, want := range []string{"┌", "┬", "┴", "│ search_started │ First query │ Count attempts │", "│ search_closed  │ Exit        │ Closed attempt │", "-timeout = 30;", "+timeout = 60;", "+new_row()", "RENDER DONE."} {
				if !strings.Contains(s.text(), want) {
					t.Fatalf("missing rendered content %q:\n%s", want, s.text())
				}
			}
			if strings.Contains(s.text(), "|---|---|---|") {
				t.Fatal("raw table delimiter in terminal")
			}
			if noColor {
				if strings.Contains(cli.snapshot(), ";48;5;") {
					t.Fatal("diff colors ignored NO_COLOR")
				}
			} else {
				for _, want := range []string{
					"\x1b[1;38;5;231;48;5;124m30\x1b[0;38;5;224;48;5;52m;\x1b[0m",
					"\x1b[1;38;5;231;48;5;34m60\x1b[0;38;5;194;48;5;22m;\x1b[0m",
					"\x1b[0;38;5;194;48;5;22m+new_row()\x1b[0m",
				} {
					if !strings.Contains(cli.snapshot(), want) {
						t.Fatalf("missing approved diff style: %q", want)
					}
				}
			}
			cli.send("check context\r")
			select {
			case request := <-requests:
				if !strings.Contains(request, `|---|---|---|`) || !strings.Contains(request, `\n+timeout = 60;`) || strings.Contains(request, "┌") || strings.Contains(request, `\u001b`) {
					t.Fatal("presentation changed canonical model context")
				}
			case <-time.After(8 * time.Second):
				t.Fatal("no continuation request")
			}
			cli.send("/exit\r")
			cli.finish(0)
		})
	}
}
