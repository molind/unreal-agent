package webchat

import (
	"context"
	"net/http"
	"time"

	"github.com/unreallabsai/unreal-agent/cmd/internal/chat"
)

func (s *Server) models(w http.ResponseWriter, r *http.Request) {
	p, err := s.project(r)
	if err != nil {
		s.fail(w, err)
		return
	}
	owner, done, err := s.acquireOwner(p, r.PathValue("session"))
	if err != nil {
		s.fail(w, err)
		return
	}
	defer done()
	ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
	defer cancel()
	respond(w, owner.Models(ctx, r.URL.Query().Get("refresh") == "1"))
}

func (s *Server) modelSettings(w http.ResponseWriter, r *http.Request) {
	var body chat.ModelSettings
	if !decode(w, r, &body) {
		return
	}
	p, err := s.project(r)
	if err != nil {
		s.fail(w, err)
		return
	}
	owner, done, err := s.acquireOwner(p, r.PathValue("session"))
	if err != nil {
		s.fail(w, err)
		return
	}
	defer done()
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	if err := owner.SetModel(ctx, body); err != nil {
		s.fail(w, err)
		return
	}
	respond(w, owner.Status())
}
