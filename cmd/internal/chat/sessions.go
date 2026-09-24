package chat

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"slices"
	"strings"

	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore/localfile"
	"github.com/unreallabsai/unreal-agent/internal/lineeditor"
)

// Read-only transcript inspection. It neither selects a session nor changes
// store observers, and is safe alongside the single coordinator writer.
func sessionTopic(ctx context.Context, store *localfile.Store, id session.ID, safe func(string) string) (string, error) {
	after := sessionstore.BeforeFirst
	hasReply := false
	for {
		page, err := store.Items(ctx, id, after, 256)
		if err != nil {
			return "", err
		}
		for _, item := range page.Items {
			if item.Kind == sessionstore.ItemModelResponse {
				hasReply = true
			}
			if input, ok := item.Data.(inbox.Input); ok && input.Kind == inbox.InputExternal {
				var text string
				if err := json.Unmarshal(input.Payload, &text); err != nil {
					return "", err
				}
				text = safe(strings.Join(strings.Fields(text), " "))
				r := []rune(text)
				if len(r) > 100 {
					text = string(r[:100]) + "…"
				}
				return "First prompt: " + text, nil
			}
		}
		if !page.More {
			break
		}
		after = page.NextAfter
	}
	if hasReply {
		return "[no user prompt]", nil
	}
	return "[empty transcript — no user messages]", nil
}
func (a *application) listSessions() error {
	sessions, err := a.store.ListSessions(a.ctx)
	if err != nil {
		if logErr := a.logs.event("storage", "list_failed", a.id, "", err); logErr != nil {
			return logErr
		}
		return a.display.print("Cannot list sessions: %v\n", err)
	}
	if err := a.display.print("Saved sessions (* current). Resume an exact ID; no automatic selection.\n"); err != nil {
		return err
	}
	if len(sessions) == 0 {
		return a.display.print("No saved sessions.\n")
	}
	for _, s := range sessions {
		topic, err := sessionTopic(a.ctx, a.store, s.ID, a.display.safe)
		if err != nil {
			if logErr := a.logs.event("storage", "topic_failed", s.ID, "", err); logErr != nil {
				return logErr
			}
			topic = "[unreadable: " + err.Error() + "]"
		}
		marker := " "
		if s.ID == a.id {
			marker = "*"
		}
		if err := a.display.print("%s %s  %s  %s\n", marker, s.ID, s.LastUpdatedAt.Format("2006-01-02 15:04:05Z"), topic); err != nil {
			return err
		}
	}
	return nil
}

func (a *application) chooseSession() error {
	if a.display.ui == nil {
		if err := a.display.print("Cursor chooser requires a capable terminal. Use /resume ID from the saved sessions below; no session selected.\n"); err != nil {
			return err
		}
		return a.listSessions()
	}
	choices, err := a.sessionChoices()
	if err != nil {
		if logErr := a.logs.event("storage", "list_failed", a.id, "", err); logErr != nil {
			return logErr
		}
		return a.display.print("Cannot list sessions: %v\n", err)
	}
	if len(choices) == 0 {
		return a.display.print("No saved sessions. This chat stays unsaved until your first message.\n")
	}
	return a.display.ui.editor.OpenSelection("Resume", choices)
}

func (a *application) sessionChoices() ([]lineeditor.Choice, error) {
	sessions, err := a.store.ListSessions(a.ctx)
	if err != nil {
		return nil, err
	}
	slices.SortFunc(sessions, func(a, b sessionstore.SessionInfo) int {
		if order := b.LastUpdatedAt.Compare(a.LastUpdatedAt); order != 0 {
			return order
		}
		return strings.Compare(string(a.ID), string(b.ID))
	})
	choices := make([]lineeditor.Choice, 0, len(sessions))
	for _, s := range sessions {
		topic, err := sessionTopic(a.ctx, a.store, s.ID, a.display.safe)
		if err != nil {
			if logErr := a.logs.event("storage", "topic_failed", s.ID, "", err); logErr != nil {
				return nil, logErr
			}
			topic = "[unreadable; Enter reports error]"
		}
		marker := " "
		if s.ID == a.id {
			marker = "*"
		}
		topic = strings.TrimPrefix(topic, "First prompt: ")
		choices = append(choices, lineeditor.Choice{
			Value:  string(s.ID),
			Label:  fmt.Sprintf("%s %s %s", marker, s.LastUpdatedAt.Format("01-02 15:04Z"), topic),
			Detail: "ID: " + string(s.ID),
		})
	}
	return choices, nil
}
