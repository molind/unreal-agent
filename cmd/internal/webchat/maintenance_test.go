package webchat

import (
	"encoding/json"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/session"
)

func sweep(t *testing.T, s *Server, now time.Time) {
	t.Helper()
	if err := s.releaseInactive(now); err != nil {
		t.Fatal(err)
	}
}

func TestIdleReleaseRetainsHistoryPinsAndRetry(t *testing.T) {
	c, m := testConfig(t)
	s := newTestServer(t, c)
	id := "11111111-1111-4111-8111-111111111111"
	base := projectSession(t, s, t.TempDir(), id)
	body := map[string]string{"id": "first", "text": "remember this"}
	ok(t, request(t, s, "POST", base+"/send", body))
	call := nextCall(t, m)
	beforeReply := time.Now()
	call.reply <- answer("remembered")
	waitHistory(t, s, base, `"state":"idle"`)
	ok(t, request(t, s, "POST", base+"/pin", map[string]bool{"pinned": true}))

	// A fresh reply resets the clock, even if the user message is old enough.
	sweep(t, s, beforeReply.Add(sessionInactivity))
	waitHistory(t, s, base, `"state":"idle"`)
	sweep(t, s, time.Now().Add(sessionInactivity-time.Minute))
	waitHistory(t, s, base, `"state":"idle"`)

	// Browsing does not retain ownership forever; the timer is message-based.
	expired := time.Now().Add(sessionInactivity + time.Minute)
	ok(t, request(t, s, "GET", base, nil))
	ok(t, request(t, s, "GET", "/api/navigation", nil))
	sweep(t, s, expired)
	waitHistory(t, s, base, `"state":"saved"`)
	w := request(t, s, "GET", base, nil)
	if !strings.Contains(w.Body.String(), "remembered") || strings.Count(w.Body.String(), `"kind":"user"`) != 1 {
		t.Fatal("lost or duplicated history", w.Body)
	}
	nav := request(t, s, "GET", "/api/navigation", nil)
	var navigation navigation
	if err := json.Unmarshal(nav.Body.Bytes(), &navigation); err != nil || len(navigation.Pinned) != 1 {
		t.Fatalf("pin lost: %s (%v)", nav.Body, err)
	}

	p := s.projects[strings.Split(base, "/")[3]]
	unlock, err := p.store.LockSession(session.ID(id))
	if err != nil {
		t.Fatal("lease retained", err)
	}
	if err := unlock(); err != nil {
		t.Fatal(err)
	}

	// A retry after cleanup still acknowledges the same persisted input once.
	ok(t, request(t, s, "POST", base+"/send", body))
	select {
	case <-m.calls:
		t.Fatal("retry replayed model work")
	default:
	}
	ok(t, request(t, s, "POST", base+"/send", map[string]string{"id": "second", "text": "continue"}))
	call = nextCall(t, m)
	call.reply <- answer("continued")
	waitHistory(t, s, base, "continued")
	w = request(t, s, "GET", base, nil)
	if strings.Count(w.Body.String(), `"kind":"user"`) != 2 {
		t.Fatal("wrong input count", w.Body)
	}
}

func TestIdleReleasePreservesRunningAndWaitingWork(t *testing.T) {
	c, m := testConfig(t)
	s := newTestServer(t, c)
	base := projectSession(t, s, t.TempDir(), "11111111-1111-4111-8111-111111111111")
	ok(t, request(t, s, "POST", base+"/send", map[string]string{"id": "first", "text": "work"}))
	call := nextCall(t, m)
	sweep(t, s, time.Now().Add(2*sessionInactivity))
	select {
	case <-call.ctx.Done():
		t.Fatal("cleanup interrupted model")
	default:
	}
	call.reply <- llm.Response{Output: []llm.Item{{Type: llm.ItemToolCall, Data: llm.ToolCall{CallID: "protected", Name: "Bash", Arguments: `{"command":"ssh example.invalid"}`}}}}
	op := waitPermission(t, s, base)
	sweep(t, s, time.Now().Add(2*sessionInactivity))
	stillPending := waitPermission(t, s, base)
	if stillPending.ApprovalID != op.ApprovalID {
		t.Fatal("cleanup canceled/replaced approval")
	}
}

func TestIdleMaintenanceRunsWithoutBrowsers(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c, _ := testConfig(t)
		s := newTestServer(t, c)
		base := projectSession(t, s, t.TempDir(), "11111111-1111-4111-8111-111111111111")
		ok(t, request(t, s, "POST", base+"/stop", map[string]string{}))
		time.Sleep(sessionInactivity - time.Minute)
		synctest.Wait()
		waitHistory(t, s, base, `"state":"stopped"`)
		// No SSE connection and no requests are needed to drive the cleanup.
		time.Sleep(2 * time.Minute)
		synctest.Wait()
		waitHistory(t, s, base, `"state":"saved"`)
	})
}

func TestIdleReleaseSerializesWithOwnerAcquisition(t *testing.T) {
	c, _ := testConfig(t)
	s := newTestServer(t, c)
	id := "11111111-1111-4111-8111-111111111111"
	base := projectSession(t, s, t.TempDir(), id)
	p := s.projects[strings.Split(base, "/")[3]]
	owner, done, err := s.acquireOwner(p, id)
	if err != nil {
		t.Fatal(err)
	}
	// Model the gap between an HTTP request's lookup and its call into Headless.
	sweep(t, s, time.Now().Add(2*sessionInactivity))
	p.mu.Lock()
	retained := p.owners[id] == owner
	p.mu.Unlock()
	done()
	if !retained {
		t.Fatal("closed an owner held by a request")
	}
	sweep(t, s, time.Now().Add(2*sessionInactivity))
	waitHistory(t, s, base, `"state":"saved"`)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-s.maintenanceDone:
	default:
		t.Fatal("maintenance outlived server")
	}
}
