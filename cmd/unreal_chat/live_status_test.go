package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

// Finite VT screen: unlike byte searches, this observes scrolling, row removal,
// cursor placement, and whether transient frames escaped into permanent history.
// The editor emits hard CRLF row endings, so resize truncates physical rows.
type liveScreen struct {
	width, height, x, y, offset int
	rows                        [][]rune
	history                     []string
	primary                     *liveScreen // Saved while the alternate viewer is active.
}

func newLiveScreen(w, h int) *liveScreen {
	s := &liveScreen{}
	s.resize(w, h)
	return s
}
func (s *liveScreen) resize(w, h int) {
	if s.primary != nil {
		s.primary.resize(w, h)
	}
	rows := make([][]rune, h)
	for i := range rows {
		rows[i] = make([]rune, w)
		if i < len(s.rows) {
			copy(rows[i], s.rows[i])
		}
	}
	s.rows, s.width, s.height = rows, w, h
	s.x, s.y = min(s.x, w-1), min(s.y, h-1)
}
func (s *liveScreen) down() {
	s.y++
	if s.y == s.height {
		s.history = append(s.history, string(s.rows[0]))
		copy(s.rows, s.rows[1:])
		s.rows[s.height-1] = make([]rune, s.width)
		s.y--
	}
}
func (s *liveScreen) update(output string) {
	b := output[s.offset:]
	i := 0
	for i < len(b) {
		if b[i] == 27 && i+1 == len(b) {
			break // Retain a fragmented escape introducer for the next PTY read.
		}
		if b[i] == 27 && i+1 < len(b) && b[i+1] == 91 {
			j := i + 2
			for j < len(b) && !(b[j] >= 64 && b[j] <= 126) {
				j++
			}
			if j == len(b) {
				break
			}
			arg := b[i+2 : j]
			n, _ := strconv.Atoi(arg)
			count := max(1, n)
			switch b[j] {
			case 65:
				s.y = max(0, s.y-count)
			case 66:
				s.y = min(s.height-1, s.y+count)
			case 67:
				s.x = min(s.width-1, s.x+count)
			case 68:
				s.x = max(0, s.x-count)
			case 'h':
				if arg == "?1049" && s.primary == nil {
					saved := *s
					s.primary = &saved
					s.rows, s.history = nil, nil
					s.x, s.y = 0, 0
					s.resize(s.width, s.height)
				}
			case 'l':
				if arg == "?1049" && s.primary != nil {
					offset := s.offset
					*s = *s.primary
					s.offset = offset
				}
			case 72:
				parts := strings.Split(arg, ";")
				row, col := 1, 1
				if len(parts) > 0 {
					value, _ := strconv.Atoi(parts[0])
					row = max(1, value)
				}
				if len(parts) > 1 {
					value, _ := strconv.Atoi(parts[1])
					col = max(1, value)
				}
				s.x, s.y = min(s.width-1, col-1), min(s.height-1, row-1)
			case 74:
				if n == 2 {
					for y := range s.rows {
						clear(s.rows[y])
					}
				} else {
					clear(s.rows[s.y][min(s.x, s.width):])
					for y := s.y + 1; y < s.height; y++ {
						clear(s.rows[y])
					}
				}
			case 75:
				clear(s.rows[s.y][min(s.x, s.width):])
			}
			i = j + 1
			continue
		}
		if !utf8.FullRuneInString(b[i:]) {
			break
		}
		r, n := utf8.DecodeRuneInString(b[i:])
		i += n
		switch r {
		case 13:
			s.x = 0
		case 10:
			s.down()
		case 8:
			s.x = max(0, s.x-1)
		default:
			if r < 32 {
				continue
			}
			if s.x == s.width {
				s.x = 0
				s.down()
			}
			s.rows[s.y][s.x] = r
			s.x++
		}
	}
	s.offset += i
}
func (s *liveScreen) text() string {
	var b strings.Builder
	for _, row := range s.rows {
		b.WriteString(strings.TrimRight(strings.ReplaceAll(string(row), "\x00", " "), " "))
		b.WriteByte(10)
	}
	return b.String()
}
func (s *liveScreen) draft(t *testing.T, draft string, fromEnd int) {
	t.Helper()
	var b strings.Builder
	for _, row := range s.rows {
		b.WriteString(strings.ReplaceAll(string(row), "\x00", " "))
	}
	text := b.String()
	index := strings.LastIndex(text, "you> "+draft)
	if index < 0 {
		t.Fatalf("missing draft %q on screen:\n%s", draft, s.text())
	}
	end := utf8.RuneCountInString(text[:index]) + utf8.RuneCountInString("you> "+draft)
	if s.y*s.width+s.x != end-fromEnd {
		t.Fatalf("cursor %d want %d:\n%s", s.y*s.width+s.x, end-fromEnd, s.text())
	}
	if strings.TrimSpace(string([]rune(text)[end:])) != "" {
		t.Fatalf("stale tail:\n%s", s.text())
	}
}

func TestTTYLiveStatusScreen(t *testing.T) {
	workspace := t.TempDir()
	requests := make(chan string, 32)
	responses := make(chan []any, 32)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		requests <- string(b)
		select {
		case out := <-responses:
			writeResponse(w, out)
		case <-r.Context().Done():
		}
	}))
	defer server.Close()
	cli := startTTY(t, []string{"TERM=xterm"}, "-provider", "openai", "-base-url", server.URL, workspace)
	cli.wait("you> ")
	screen := newLiveScreen(40, 24)
	check := func() {
		before := len(screen.history)
		text := cli.snapshot()
		start := screen.offset
		screen.update(text)
		for _, row := range screen.history[before:] {
			if regexp.MustCompile(`^[*+.\-] .* (running|canceling|model / input)|^\+[0-9]+ more pending|^── `).MatchString(row) {
				t.Fatalf("transient at %dx%d, row %q, update %q", screen.width, screen.height, row, text[start:])
			}
		}
	}
	waitScreen := func(want string) {
		t.Helper()
		until := time.Now().Add(8 * time.Second)
		for time.Now().Before(until) {
			check()
			if strings.Contains(screen.text(), want) {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("missing %q on screen:\n%s", want, screen.text())
	}
	request := func() string {
		t.Helper()
		select {
		case b := <-requests:
			return b
		case <-time.After(8 * time.Second):
			t.Fatal("no request")
			return ""
		}
	}
	cli.send("tasks\r")
	request()
	originalSession := regexp.MustCompile(`Session: ([a-zA-Z0-9-]+)`).FindStringSubmatch(cli.snapshot())[1]
	waitScreen("model / input pending")
	first := screen.text()
	time.Sleep(250 * time.Millisecond)
	check()
	if first == screen.text() {
		t.Fatal("status did not animate")
	}
	// Three independent processes, with completion order controlled locally.
	responses <- []any{toolOutput("one", "while [ ! -f finish-one ]; do sleep .05; done; printf ok"), toolOutput("two", "while [ ! -f finish-two ]; do sleep .05; done; exit 7"), toolOutput("three", "sleep 60")}
	cli.wait("Bash: sleep 60")
	draft := "Прывітанне, беларускі тэкст ABC"
	cli.send(draft + "\x1b[D\x1b[D")
	time.Sleep(350 * time.Millisecond)
	check()
	screen.draft(t, draft, 2)
	if strings.Count(screen.text(), "running") != 3 {
		t.Fatalf("not vertical tasks:\n%s", screen.text())
	}
	if strings.Contains(cli.snapshot(), "Working —") {
		t.Fatal("Working transcript pollution")
	}
	if err := os.WriteFile(filepath.Join(workspace, "finish-two"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	cli.wait("failed (exit 7)")
	request()
	responses <- []any{messageOutput("out of order failure received")}
	cli.wait("out of order failure received")
	time.Sleep(250 * time.Millisecond)
	check()
	screen.draft(t, draft, 2)
	if strings.Count(screen.text(), "running") != 2 {
		t.Fatalf("finished row remains:\n%s", screen.text())
	}
	if err := os.WriteFile(filepath.Join(workspace, "finish-one"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	cli.wait("\x1b[32m✓\x1b[0m Bash: while")
	request()
	responses <- []any{messageOutput("success received")}
	cli.wait("success received")
	cli.send("\x03")
	cli.wait("Draft cleared.")
	check()
	screen.draft(t, "", 0)
	if !strings.Contains(screen.text(), "running") {
		t.Fatal("clearing a draft stopped the remaining tool")
	}
	cli.send("\x03")
	cli.wait("Stopped.")
	time.Sleep(250 * time.Millisecond)
	check()
	screen.draft(t, "", 0)
	if strings.Contains(screen.text(), "running") || strings.Contains(screen.text(), "model / input") {
		t.Fatalf("not idle:\n%s", screen.text())
	}
	text := cli.snapshot()
	if strings.Count(text, "\x1b[32m✓\x1b[0m Bash: while") != 1 {
		t.Fatal("missing or duplicate compact success notice")
	}
	if strings.Contains(text, " completed —") || strings.Contains(text, "\x1b[32mBash:") {
		t.Fatal("noisy success prefix or whole-command color")
	}
	for _, pair := range []struct{ code, marker, state string }{{"31", "✗", "failed (exit 7)"}, {"33", "–", "canceled"}} {
		if strings.Count(text, "\x1b["+pair.code+"m"+pair.marker+"\x1b[0m Bash:") != 1 {
			t.Fatalf("completion/color %s is missing or duplicated", pair.state)
		}
		re := regexp.MustCompile(regexp.QuoteMeta(pair.state) + " · ([a-zA-Z0-9-]{8})")
		if matches := re.FindAllStringSubmatch(text, -1); len(matches) != 1 {
			t.Fatalf("state/short identity %s count %d", pair.state, len(matches))
		}
	}
	cli.send("Ж\r")
	if got := lastUserText(t, request()); got != "Ж" {
		t.Fatalf("cursor/input changed: %q", got)
	}
	responses <- []any{messageOutput("draft received")}
	cli.wait("draft received")
	// Resize before new work, then exercise the same live area at narrow/short sizes.
	check()
	if err := cli.resize(8, 32); err != nil {
		t.Fatal(err)
	}
	screen.resize(32, 8)
	_ = cli.cmd.Process.Signal(unix.SIGWINCH)
	time.Sleep(250 * time.Millisecond)
	check()
	cli.send("overflow\r")
	request()
	var tools []any
	for i := 0; i < 9; i++ {
		tools = append(tools, toolOutput(fmt.Sprint(i), fmt.Sprintf("sleep %d", 60+i)))
	}
	responses <- tools
	waitScreen("more pending")
	smallDraft := "праверка ABC"
	cli.send(smallDraft + "\x1b[D\x1b[D")
	time.Sleep(300 * time.Millisecond)
	check()
	screen.draft(t, smallDraft, 2)
	if strings.Count(screen.text(), "running") != 4 || !strings.Contains(screen.text(), "+5 more pending") {
		t.Fatalf("overflow:\n%s", screen.text())
	}
	// Shrink while active with a cursor in the middle, then grow it again.
	if err := cli.resize(8, 24); err != nil {
		t.Fatal(err)
	}
	screen.resize(24, 8)
	_ = cli.cmd.Process.Signal(unix.SIGWINCH)
	time.Sleep(300 * time.Millisecond)
	check()
	screen.draft(t, smallDraft, 2)
	if err := cli.resize(12, 40); err != nil {
		t.Fatal(err)
	}
	screen.resize(40, 12)
	_ = cli.cmd.Process.Signal(unix.SIGWINCH)
	time.Sleep(300 * time.Millisecond)
	check()
	screen.draft(t, smallDraft, 2)
	// /status retains every operation, not just the bounded live subset. Use a
	// large report viewport here; dedicated viewer tests exercise pagination.
	if err := cli.resize(40, 120); err != nil {
		t.Fatal(err)
	}
	screen.resize(120, 40)
	_ = cli.cmd.Process.Signal(unix.SIGWINCH)
	time.Sleep(150 * time.Millisecond)
	offset := len(cli.snapshot())
	cli.send("\x05\x15/status\r")
	until := time.Now().Add(8 * time.Second)
	for {
		text := cli.snapshot()[offset:]
		if len(regexp.MustCompile(`  [a-zA-Z0-9-]+ running — Bash: sleep`).FindAllString(text, -1)) == 9 {
			break
		}
		if time.Now().After(until) {
			t.Fatalf("/status omitted operations: %s", text)
		}
		time.Sleep(10 * time.Millisecond)
	}
	cli.closeViewer()
	// A new session cancels work and clears live rows without stale tails.
	before := strings.Count(cli.snapshot(), "Selected session")
	cli.send("/new\r")
	until = time.Now().Add(8 * time.Second)
	for strings.Count(cli.snapshot(), "Selected session") == before {
		if time.Now().After(until) {
			t.Fatal("/new did not complete")
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(250 * time.Millisecond)
	check()
	if strings.Contains(screen.text(), "running") || strings.Contains(screen.text(), "pending") {
		t.Fatalf("new did not clear:\n%s", screen.text())
	}
	// Resuming that canceled session must not bring the old live list back.
	cli.send("/resume " + originalSession + "\r")
	until = time.Now().Add(8 * time.Second)
	for strings.Count(cli.snapshot(), "Selected session") == before+1 {
		if time.Now().After(until) {
			t.Fatal("/resume did not complete")
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(250 * time.Millisecond)
	check()
	if strings.Contains(screen.text(), "running") || strings.Contains(screen.text(), "pending") {
		t.Fatalf("resume restored stale status:\n%s", screen.text())
	}
	screen.draft(t, "", 0)
	cli.send("/exit\r")
	cli.finish(0)
	check()
	for _, row := range screen.history {
		if regexp.MustCompile(`^[*+.\-] .* (running|canceling|model / input)|^\+[0-9]+ more pending|^── `).MatchString(row) {
			t.Fatalf("transient row in scrollback: %q", row)
		}
	}
}

func TestLiveScreenFragmentedControls(t *testing.T) {
	text := "transcript\r\n* running\r\nyou> беларускі\x1b[2D\x1b[1A\x1b[5D\x1b[Jidle\r\nyou> draft\x1b[D"
	whole, fragmented := newLiveScreen(24, 6), newLiveScreen(24, 6)
	whole.update(text)
	for i := 1; i <= len(text); i++ {
		fragmented.update(text[:i])
	}
	if whole.text() != fragmented.text() || whole.x != fragmented.x || whole.y != fragmented.y || whole.offset != fragmented.offset {
		t.Fatalf("PTY chunk boundaries changed screen: whole=%q fragmented=%q", whole.text(), fragmented.text())
	}
}
