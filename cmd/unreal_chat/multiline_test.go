package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestTTYOptionReturnEditsWithoutSubmitting(t *testing.T) {
	requests := make(chan string, 8)
	responses := make(chan string, 8)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		requests <- string(data)
		select {
		case reply := <-responses:
			writeResponse(w, []any{messageOutput(reply)})
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(server.Close)
	cli := startTTY(t, []string{"TERM=xterm", "NO_COLOR=1"}, "-provider", "openai", "-base-url", server.URL, t.TempDir())
	cli.wait("you> ")
	s := &pickerScreen{t, cli, newLiveScreen(40, 24)}
	s.resizeTo(60, 12)
	request := func(want string) {
		t.Helper()
		select {
		case body := <-requests:
			if lastUserText(t, body) != want {
				t.Fatalf("message changed: %q", lastUserText(t, body))
			}
		case <-time.After(8 * time.Second):
			t.Fatal("no request")
		}
	}
	cli.send("start\r")
	request("start")
	cli.send("першы\x1b\r")
	until := time.Now().Add(8 * time.Second)
	for {
		s.check()
		if s.x == 0 && strings.TrimSpace(strings.ReplaceAll(string(s.rows[s.y]), "\x00", " ")) == "" {
			break
		}
		if time.Now().After(until) {
			t.Fatalf("Option+Return did not move to column zero:\n%s", s.text())
		}
		time.Sleep(5 * time.Millisecond)
	}
	cli.send("другі\x1b[A!")
	s.wait("першы!")
	s.wait("другі")
	select {
	case <-requests:
		t.Fatal("Option Return submitted")
	case <-time.After(100 * time.Millisecond):
	}
	responses <- "Async while editing"
	s.wait("Async while editing")
	s.resizeTo(32, 8)
	cli.send("\x1b[B?\r")
	request("першы!\nдругі?")
	responses <- "Received multiline"
	cli.wait("Received multiline")
	cli.send("\x10\x01X\x05Y\r")
	request("Xпершы!\nдругі?Y")
	responses <- "History received"
	cli.wait("History received")
	cli.send("/exit\x1b\rnot a command\r")
	request("/exit\nnot a command")
	responses <- "Literal received"
	cli.wait("Literal received")
	cli.send("discard\x1b\rall lines\x03")
	cli.wait("Draft cleared.")
	s.waitPrompt()
	cli.send("/exit\r")
	cli.finish(0)
}
