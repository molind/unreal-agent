package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func waitViewerScreen(t *testing.T, cli *ttyCLI, screen *liveScreen, test func() bool) {
	t.Helper()
	until := time.Now().Add(8 * time.Second)
	for time.Now().Before(until) {
		screen.update(cli.snapshot())
		if test() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("viewer screen condition not reached; visible:\n%s", screen.text())
}
func TestTTYReportViewerRestoresHistoryAndQueuesLiveOutput(t *testing.T) {
	requests := make(chan struct{}, 8)
	responses := make(chan []any, 8)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		requests <- struct{}{}
		select {
		case output := <-responses:
			writeResponse(w, output)
		case <-r.Context().Done():
		}
	}))
	defer server.Close()
	cli := startTTY(t, []string{"TERM=xterm-256color", "NO_COLOR=1"}, "-provider", "openai", "-base-url", server.URL, t.TempDir())
	cli.wait("you> ")
	screen := newLiveScreen(40, 24)
	cli.send("first\r")
	select {
	case <-requests:
	case <-time.After(8 * time.Second):
		t.Fatal("no first request")
	}
	responses <- []any{messageOutput("PRIMARY-HISTORY-MARKER")}
	cli.wait("PRIMARY-HISTORY-MARKER")
	cli.send("second\r")
	select {
	case <-requests:
	case <-time.After(8 * time.Second):
		t.Fatal("no second request")
	}
	cli.send("/help\r")
	waitViewerScreen(t, cli, screen, func() bool { return screen.primary != nil && strings.Contains(screen.text(), "Help (snapshot)") })
	primary := screen.primary.text() + strings.Join(screen.primary.history, "\n")
	if !strings.Contains(primary, "PRIMARY-HISTORY-MARKER") {
		t.Fatal("opening viewer erased primary transcript")
	}
	before := screen.text()
	cli.send("\x1b[6~")
	waitViewerScreen(t, cli, screen, func() bool { return screen.text() != before })
	if strings.Contains(screen.primary.text(), "Help (snapshot)") {
		t.Fatal("report leaked into primary screen")
	}
	// Paste must neither submit a command nor replace the draft after dismissal.
	cli.send("\x1b[200~/exit\nq\x03\x1b[201~\r")
	responses <- []any{messageOutput("QUEUED-REPLY-MARKER")}
	waitViewerScreen(t, cli, screen, func() bool { return strings.Contains(screen.text(), "new bytes") })
	if strings.Contains(screen.text(), "QUEUED-REPLY-MARKER") || strings.Contains(screen.primary.text(), "QUEUED-REPLY-MARKER") {
		t.Fatal("async output overwrote viewer/primary before close")
	}
	if err := cli.resize(12, 32); err != nil {
		t.Fatal(err)
	}
	screen.resize(32, 12)
	if err := cli.cmd.Process.Signal(unix.SIGWINCH); err != nil {
		t.Fatal(err)
	}
	cli.send("\x1b[F")
	waitViewerScreen(t, cli, screen, func() bool { return screen.primary != nil && strings.Contains(screen.text(), "new bytes") })
	cli.closeViewer()
	waitViewerScreen(t, cli, screen, func() bool {
		return screen.primary == nil && screen.x == 5 && strings.HasPrefix(string(screen.rows[screen.y]), "you> ")
	})
	transcript := strings.Join(screen.history, "\n") + screen.text()
	if !strings.Contains(transcript, "QUEUED-REPLY-MARKER") || strings.Contains(transcript, "Help (snapshot)") || strings.Contains(transcript, "Esc back |") {
		t.Fatal("history was polluted or queued output was lost")
	}
	if regexp.MustCompile(`\x1b\[[0-9;]*m`).MatchString(cli.snapshot()) {
		t.Fatal("NO_COLOR emitted style escapes")
	}
	select {
	case <-requests:
		t.Fatal("viewer keys/paste sent a model request")
	default:
	}
	cli.send("/exit\r")
	cli.finish(0)
}
func TestTTYViewerCtrlCAndEOFCleanup(t *testing.T) {
	for _, end := range []string{"interrupt", "EOF"} {
		t.Run(end, func(t *testing.T) {
			cli := startTTY(t, []string{"TERM=xterm", "NO_COLOR=1"}, "-provider", "ollama", t.TempDir())
			cli.wait("you> ")
			cli.send("/help\r")
			cli.wait("Help (snapshot)")
			if end == "interrupt" {
				cli.send("\x03")
				cli.wait("\x1b[?1049l")
				cli.send("/status\r")
				cli.wait("Status (snapshot)")
				cli.closeViewer()
				cli.send("/exit\r")
			} else {
				cli.send("\x04")
			}
			cli.finish(0)
			if strings.Count(cli.snapshot(), "\x1b[?1049h") != strings.Count(cli.snapshot(), "\x1b[?1049l") {
				t.Fatal("alternate screen not restored")
			}
		})
	}
}
