package localfile

import (
	"errors"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/session"
)

func TestSessionTitleIsIndependentMetadata(t *testing.T) {
	dir := t.TempDir()
	s := sqliteStore(t, dir)
	ctx := t.Context()
	for _, id := range []session.ID{"first", "second"} {
		if _, err := s.Create(ctx, id); err != nil {
			t.Fatal(err)
		}
		if err := s.AppendInput(ctx, id, sqlInput(string(id), "original message")); err != nil {
			t.Fatal(err)
		}
	}
	before, err := s.ListSessions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	history, err := s.Items(ctx, "first", 0, 64)
	if err != nil {
		t.Fatal(err)
	}

	other := sqliteStore(t, dir)
	lease, err := other.LockSession("first")
	if err != nil {
		t.Fatal(err)
	}
	defer lease()
	// Naming must work while the runtime owns the session, without touching it.
	for range 2 {
		if err := s.SetSessionTitle(ctx, "first", "  Беларуская\n назва\t🚀  "); err != nil {
			t.Fatal(err)
		}
	}
	titles, err := other.SessionTitles(ctx)
	if err != nil || !reflect.DeepEqual(titles, map[session.ID]string{"first": "Беларуская назва 🚀"}) {
		t.Fatalf("titles=%v err=%v", titles, err)
	}
	after, err := s.ListSessions(ctx)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("rename changed list timestamps: %v", err)
	}
	afterHistory, err := s.Items(ctx, "first", 0, 64)
	if err != nil || !reflect.DeepEqual(history, afterHistory) {
		t.Fatalf("rename changed history: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = sqliteStore(t, dir)
	titles, err = s.SessionTitles(ctx)
	if err != nil || titles["first"] != "Беларуская назва 🚀" {
		t.Fatalf("title lost on reopen: %v %v", titles, err)
	}
	if err := s.SetSessionTitle(ctx, "first", " \n\t "); err != nil {
		t.Fatal(err)
	}
	titles, err = other.SessionTitles(ctx)
	if err != nil || len(titles) != 0 {
		t.Fatalf("reset failed: %v %v", titles, err)
	}
}

func TestSessionTitleConcurrentDeletion(t *testing.T) {
	s := sqliteStore(t, t.TempDir())
	other := sqliteStore(t, s.directory)
	ctx := t.Context()
	for i := range 20 {
		id := session.ID("session-" + strconv.Itoa(i))
		if _, err := s.Create(ctx, id); err != nil {
			t.Fatal(err)
		}
		start := make(chan struct{})
		renamed, deleted := make(chan error, 1), make(chan error, 1)
		go func() { <-start; renamed <- s.SetSessionTitle(ctx, id, "Concurrent title") }()
		go func() { <-start; deleted <- other.DeleteSession(ctx, id) }()
		close(start)
		renameErr, deleteErr := <-renamed, <-deleted
		if renameErr != nil && !errors.Is(renameErr, os.ErrNotExist) {
			t.Fatal(renameErr)
		}
		if deleteErr != nil {
			t.Fatal(deleteErr)
		}
	}
	var count int
	if err := s.database.QueryRowContext(ctx, "SELECT count(*) FROM metadata WHERE namespace=?", sessionTitleNamespace).Scan(&count); err != nil || count != 0 {
		t.Fatalf("orphan titles after concurrent deletion: %d, %v", count, err)
	}
}

func TestSessionTitleValidationAndDeletion(t *testing.T) {
	s := sqliteStore(t, t.TempDir())
	ctx := t.Context()
	if _, err := s.Create(ctx, "id"); err != nil {
		t.Fatal(err)
	}
	boundary := strings.Repeat("🚀", MaxSessionTitleLength)
	if err := s.SetSessionTitle(ctx, "id", boundary); err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{boundary + "а", "bad\x00title", "bad\x1btitle", string([]byte{0xff})} {
		if err := s.SetSessionTitle(ctx, "id", value); !errors.Is(err, ErrInvalidSessionTitle) {
			t.Fatalf("invalid title accepted: %v", err)
		}
	}
	for _, value := range []string{"name", ""} {
		if err := s.SetSessionTitle(ctx, "missing", value); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("renamed absent session: %v", err)
		}
	}
	// A failed transaction leaves both the title and the session intact.
	if _, err := s.database.Exec("CREATE TRIGGER refuse_title_delete BEFORE DELETE ON sessions BEGIN SELECT RAISE(ABORT,'injected'); END"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteSession(ctx, "id"); err == nil {
		t.Fatal("expected deletion error")
	}
	titles, err := s.SessionTitles(ctx)
	if err != nil || titles["id"] != boundary {
		t.Fatalf("rollback lost title: %v", err)
	}
	if _, err := s.database.Exec("DROP TRIGGER refuse_title_delete"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteSession(ctx, "id"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSessionTitle(ctx, "id", "late rename"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("rename resurrected deleted session", err)
	}
	// The title is actually removed, not just hidden by SessionTitles' join.
	if _, err := s.Create(ctx, "id"); err != nil {
		t.Fatal(err)
	}
	titles, err = s.SessionTitles(ctx)
	if err != nil || len(titles) != 0 {
		t.Fatalf("recreated session inherited title: %v %v", titles, err)
	}
}
