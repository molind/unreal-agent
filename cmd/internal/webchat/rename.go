package webchat

import (
	"errors"
	"net/http"
	"os"

	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore/localfile"
)

func (s *Server) renameSession(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Title *string `json:"title"`
	}
	if !decode(w, r, &body) {
		return
	}
	if body.Title == nil {
		http.Error(w, "session title is required", http.StatusBadRequest)
		return
	}
	p, err := s.project(r)
	if err == nil {
		err = func() error {
			p.mu.Lock()
			defer p.mu.Unlock()
			if err := s.open(p); err != nil {
				return err
			}
			return p.store.SetSessionTitle(r.Context(), session.ID(r.PathValue("session")), *body.Title)
		}()
	}
	if errors.Is(err, localfile.ErrInvalidSessionTitle) {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if errors.Is(err, os.ErrNotExist) {
		http.Error(w, "conversation no longer exists", http.StatusNotFound)
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	s.signal()
	respond(w, map[string]bool{"renamed": true})
}
