package chat

import (
	"bytes"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
)

func bracketed(s string) string { return "\x1b[200~" + s + "\x1b[201~" }

func editTerminal(t *testing.T, keys string) ([]line, string, *terminalUI) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	var out bytes.Buffer
	u := &terminalUI{input: r, output: &out, failures: make(chan error, 1)}
	u.initEditor()
	written := make(chan error, 1)
	go func() { _, err := io.WriteString(w, keys); _ = w.Close(); written <- err }()
	var lines []line
	for {
		l, err := u.readLine()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		lines = append(lines, l)
	}
	if err := <-written; err != nil {
		t.Fatal(err)
	}
	return lines, out.String(), u
}
func TestTerminalFaithfulPasteAndEditing(t *testing.T) {
	code := "\tif ready {\n\t\tprintln(\"прывітанне\")\n\t}\n  tail  \n"
	long := "LONG-PASTE-" + strings.Repeat("б", 6000) + "-END"
	for _, test := range []struct {
		name, keys string
		want       []line
		folded     bool
	}{
		{"indentation", bracketed(code) + "\r", []line{{text: code, literal: true}}, true},
		{"long", bracketed(long) + "\r", []line{{text: long, literal: true}}, true},
		{"slash", bracketed("/exit\n\t/not a command\n") + "\r", []line{{text: "/exit\n\t/not a command\n", literal: true}}, true},
		{"mixed and recalled", "before!\x1b[D" + bracketed(code) + "after\r\x1b[A\x01[\x05]\r", []line{{text: "before" + code + "after!", literal: true}, {text: "[before" + code + "after!]", literal: true}}, true},
		{"multiple", "A" + bracketed(code) + "B" + bracketed(long) + "C\r", []line{{text: "A" + code + "B" + long + "C", literal: true}}, true},
		{"atomic backspace", "before" + bracketed(long) + "\x7fafter\r", []line{{text: "beforeafter"}}, true},
		{"atomic delete", "before" + bracketed(long) + "\x1b[D\x1b[3~after\r", []line{{text: "beforeafter"}}, true},
		{"whitespace", bracketed("\t \n\n") + "\r", []line{{text: "\t \n\n", literal: true}}, true},
		{"typed command, pasted argument", "/resume " + bracketed("saved-id") + "\r", []line{{text: "/resume saved-id"}}, false},
		{"pasted verb", "/" + bracketed("exit") + "\r", []line{{text: "/exit"}}, false},
		{"typed verb, multiline argument", "/resume " + bracketed("id\n/stop") + "\r", []line{{text: "/resume id\n/stop", literal: true}}, true},
		{"pasted command with trailing newline", bracketed(" /status\n\n") + "\r", []line{{text: " /status\n\n"}}, true},
		{"typed command with copied line", "/resume " + bracketed("saved-id\r\n") + "\r", []line{{text: "/resume saved-id\r\n"}}, true},
		{"private literal", bracketed("\ue000\uf8ff") + "\r", []line{{text: "\ue000\uf8ff", literal: true}}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, out, _ := editTerminal(t, test.keys)
			if len(got) != len(test.want) {
				t.Fatalf("got %d messages, want %d; tail: %s", len(got), len(test.want), out[max(0, len(out)-200):])
			}
			for i, want := range test.want {
				if got[i] != want {
					t.Fatalf("message %d changed: got %q (%d bytes), want %q (%d bytes)", i, got[i].text, len(got[i].text), want.text, len(want.text))
				}
			}
			for _, r := range out {
				if attachmentRune(r) {
					t.Fatal("private marker leaked instead of visible attachment cell")
				}
			}
			if strings.Contains(out, "Paste attached as ▣:") != test.folded {
				t.Fatal("incorrect inline/folded paste presentation")
			}
		})
	}
}
func TestTerminalLimitsRejectWholeDraft(t *testing.T) {
	for _, test := range []struct{ name, keys, reason string }{
		{"paste bytes", bracketed(strings.Repeat("x", maxMessageBytes+1)), "exceeds 1 MiB"},
		{"aggregate", bracketed(strings.Repeat("x", maxMessageBytes)) + "z", "exceeds 1 MiB"},
		{"typed cells", strings.Repeat("б", maxEditorRunes+1), "exceeds 4096 cells"},
		{"inline paste cells", strings.Repeat("б", maxEditorRunes-1) + bracketed("xy"), "exceeds 4096 cells"},
		{"inline paste bytes", bracketed(strings.Repeat("x", maxMessageBytes)) + bracketed("small"), "exceeds 1 MiB"},
		{"invalid UTF8", bracketed("start\xfftail"), "not valid UTF-8"},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, out, _ := editTerminal(t, "previous\r"+test.keys+"\rsafe\r\x1b[A\r")
			if len(got) != 3 || got[0].text != "previous" || got[1].text != "safe" || got[2].text != "safe" {
				t.Fatalf("rejected draft submitted/recalled: %d messages", len(got))
			}
			if !strings.Contains(out, "Input rejected:") || !strings.Contains(out, test.reason) || !strings.Contains(out, "Nothing from this draft will be sent") {
				t.Fatal("no explicit rejection")
			}
		})
	}
	for _, text := range []string{strings.Repeat("б", maxEditorRunes), strings.Repeat("x", maxMessageBytes-4) + "-END"} {
		keys := text + "\r"
		if len(text) > maxEditorRunes*2 {
			keys = bracketed(text) + "\r"
		}
		got, _, _ := editTerminal(t, keys)
		if len(got) != 1 || got[0].text != text {
			t.Fatalf("supported boundary changed (%d bytes)", len(text))
		}
	}
}
func TestTerminalPasteControlsAndCancellation(t *testing.T) {
	payload := "\tAPI_KEY=private\n\x1b[2J\x1b\x1b[A\x03\x00\r\n-end"
	got, out, u := editTerminal(t, bracketed(payload)+"\r"+"before\x1b\x03"+bracketed(payload)+"\x03after\r")
	if len(got) != 4 || got[0].text != payload || !got[0].literal || got[1] != (line{interrupt: true, cleared: true}) || got[2] != (line{interrupt: true, cleared: true}) || got[3].text != "after" {
		t.Fatalf("paste/interrupt behavior: %#v", got)
	}
	if strings.Contains(out, "private") || strings.Contains(out, "\x1b[2J") {
		t.Fatal("paste contents executed/echoed")
	}
	if u.history.Len() != 2 {
		t.Fatal("cleared input was remembered")
	}
}

func TestTerminalInterruptClearsDraftAndPreservesKeyOrder(t *testing.T) {
	for _, draft := range []string{"беларускі draft", "   ", bracketed("/tmp/short-path"), "before" + bracketed("line one\nline two") + "after", strings.Repeat("x", maxEditorRunes+1), "\xd0"} {
		got, _, _ := editTerminal(t, draft+"\x03\x03fresh\r")
		if len(got) != 3 || got[0] != (line{interrupt: true, cleared: true}) || got[1] != (line{interrupt: true}) || got[2].text != "fresh" {
			t.Fatalf("interrupts lost, coalesced, or retained the draft: %#v", got)
		}
	}
	got, _, u := editTerminal(t, bracketed("saved\nblock")+"\r"+bracketed("discarded\nblock")+"\x03\x1b[A\r")
	if len(got) != 3 || got[0].text != "saved\nblock" || got[2].text != got[0].text || len(u.attachments) != 1 {
		t.Fatal("clearing a draft damaged submitted history or retained its attachment")
	}
}

func TestTerminalAttachmentsExpireWithLibraryHistory(t *testing.T) {
	_, _, u := editTerminal(t, bracketed("original\n\tblock")+"\r"+strings.Repeat("next\r", 100))
	if u.history.Len() != 100 || len(u.attachments) != 0 {
		t.Fatal("expired history retained paste contents")
	}
}

func TestTerminalRejectedTypingHasBoundedEcho(t *testing.T) {
	got, out, _ := editTerminal(t, strings.Repeat("б", maxEditorRunes+256)+"\rnext\r")
	if len(got) != 1 || got[0].text != "next" {
		t.Fatal("rejected input submitted")
	}
	if len(out) > 100_000 {
		t.Fatalf("rejection re-echoed the whole draft per dropped key: %d bytes", len(out))
	}
}

func TestTerminalShortPasteIsEditableAndNeverSubmits(t *testing.T) {
	for _, test := range []struct{ name, keys, want string }{
		{"path", bracketed("/Users/evgen/my project/file.go") + "\x7fX\r", "/Users/evgen/my project/file.gX"},
		{"delete in middle", bracketed("abcd") + "\x1b[D\x1b[D\x1b[3~X\r", "abXd"},
		{"Cyrillic", bracketed("прывітанне") + "\x7fі\r", "прывітанні"},
		{"mixed", "before" + bracketed(" /tmp/path ") + "after\r", "before /tmp/path after"},
		{"command", bracketed("/status") + "\r", "/status"},
		{"empty", "keep" + bracketed("") + "\r", "keep"},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, out, _ := editTerminal(t, test.keys)
			if len(got) != 1 || got[0].text != test.want || got[0].literal {
				t.Fatalf("short paste was not normal editable text: %#v", got)
			}
			if strings.Contains(out, "Paste attached") {
				t.Fatal("short paste was folded")
			}
		})
	}
	got, _, _ := editTerminal(t, bracketed("/tmp/path"))
	if len(got) != 0 {
		t.Fatal("paste submitted without Enter")
	}
	got, _, _ = editTerminal(t, bracketed("/tmp/path")+"\r\x1b[A\x7fX\r")
	if len(got) != 2 || got[1].text != "/tmp/patX" {
		t.Fatal("short paste history lost character editing")
	}
}

func TestTerminalPasteThresholdAndSafeFolding(t *testing.T) {
	for _, test := range []struct {
		text string
		fold bool
	}{
		{strings.Repeat("б", maxInlinePasteRunes), false},
		{strings.Repeat("б", maxInlinePasteRunes+1), true},
		{"/Users/name/path\n", true},
		{"/tmp/" + strings.Repeat("x", maxInlinePasteRunes), true},
		{"one\ttwo", true},
		{"one\ntwo", true},
		{"text\x1b[2J", true},
		{"text\u202e", true},
		{"\ue000", true},
	} {
		got, out, _ := editTerminal(t, bracketed(test.text)+"\r")
		if len(got) != 1 || got[0].text != test.text || got[0].literal != test.fold {
			t.Fatalf("paste changed: %#v, want %q folded=%v", got, test.text, test.fold)
		}
		if strings.Contains(out, "Paste attached") != test.fold || strings.Contains(out, "\x1b[2J") || strings.ContainsRune(out, '\u202e') {
			t.Fatal("wrong/unsafe paste display")
		}
	}
}

func TestTerminalOptionWordMotion(t *testing.T) {
	for _, keys := range [][2]string{{"\x1bb", "\x1bf"}, {"\x1b[1;3D", "\x1b[1;3C"}, {"\x1b[1;9D", "\x1b[1;9C"}} {
		got, _, _ := editTerminal(t, "адзін два тры"+keys[0]+keys[0]+"X"+keys[1]+"Y\r")
		if len(got) != 1 || got[0].text != "адзін Xдва Yтры" {
			t.Fatalf("word motion did not pass through the input transport: %#v", got)
		}
		payload := "literal" + keys[0] + keys[1]
		got, _, _ = editTerminal(t, bracketed(payload)+"\r")
		if len(got) != 1 || got[0].text != payload || !got[0].literal {
			t.Fatal("Option sequences in pasted text were interpreted as keys")
		}
		payload = "folded\nblock"
		got, _, _ = editTerminal(t, "before "+bracketed(payload)+" after"+keys[0]+keys[0]+"X"+keys[1]+"Y\r")
		if len(got) != 1 || got[0].text != "before X"+payload+" Yafter" {
			t.Fatal("word motion damaged a folded paste")
		}
	}
}
