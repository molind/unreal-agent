package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestTTYOptionWordMotionAcrossOutputAndResize(t *testing.T) {
	type call struct {
		body  string
		reply chan string
	}
	calls := make(chan call, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		c := call{string(body), make(chan string, 1)}
		calls <- c
		select {
		case response := <-c.reply:
			writeResponse(w, []any{messageOutput(response)})
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(server.Close)
	cli := startTTY(t, []string{"TERM=xterm"}, "-provider", "openai", "-base-url", server.URL, t.TempDir())
	cli.wait("you> ")
	s := &pickerScreen{t, cli, newLiveScreen(40, 24)}
	request := func() call {
		t.Helper()
		select {
		case c := <-calls:
			return c
		case <-time.After(8 * time.Second):
			t.Fatal("missing model request")
			return call{}
		}
	}
	waitDraft := func(draft string, fromEnd int) {
		t.Helper()
		until := time.Now().Add(8 * time.Second)
		for time.Now().Before(until) {
			s.check()
			if s.x == 5+utf8.RuneCountInString(draft)-fromEnd && strings.HasPrefix(string(s.rows[s.y]), "you> "+draft) {
				s.draft(t, draft, fromEnd)
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("word cursor did not reach the expected position:\n%s", s.text())
	}
	for i, keys := range [][2]string{{"\x1bb", "\x1bf"}, {"\x1b[1;3D", "\x1b[1;3C"}, {"\x1b[1;9D", "\x1b[1;9C"}} {
		s.resizeTo(80, 14)
		cli.send(fmt.Sprintf("start-%d\r", i))
		c := request()
		cli.send("адзін два тры" + keys[0] + keys[0])
		waitDraft("адзін два тры", 7)
		response := fmt.Sprintf("Async reply %d", i)
		c.reply <- response
		s.wait(response)
		waitDraft("адзін два тры", 7)
		s.resizeTo(32, 12)
		waitDraft("адзін два тры", 7)
		cli.send("X" + keys[1])
		waitDraft("адзін Xдва тры", 3)
		cli.send("Y\r")
		c = request()
		if got := lastUserText(t, c.body); got != "адзін Xдва Yтры" {
			t.Fatalf("Option keys changed payload or did not move by words: %q", got)
		}
		response = fmt.Sprintf("Accepted %d", i)
		c.reply <- response
		s.wait(response)
		s.waitPrompt()
	}
	cli.send("/exit\r")
	cli.finish(0)
}
