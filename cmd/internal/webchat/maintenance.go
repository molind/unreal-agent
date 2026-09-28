package webchat

import (
	"errors"
	"log"
	"time"
)

const sessionInactivity = time.Hour

// Maintenance belongs to the server, not an SSE connection or an open browser.
func (s *Server) maintain() {
	defer close(s.maintenanceDone)
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case now := <-ticker.C:
			if err := s.releaseInactive(now); err != nil {
				log.Printf("web session cleanup: %s", s.safe(err.Error()))
			}
		case <-s.maintenanceStop:
			return
		}
	}
}

func (s *Server) releaseInactive(now time.Time) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	projects := make([]*workspace, 0, len(s.projects))
	for _, p := range s.projects {
		projects = append(projects, p)
	}
	s.mu.Unlock()
	var result error
	changed := false
	for _, p := range projects {
		p.mu.Lock()
		for id, owner := range p.owners {
			// An action may have acquired its owner but not yet entered Headless.
			// Do not close that owner out from under a send/resume/approval.
			if p.inFlight[id] != 0 {
				continue
			}
			released, err := owner.CloseIfInactive(now.Add(-sessionInactivity))
			result = errors.Join(result, err)
			if released {
				delete(p.owners, id)
				changed = true
			}
		}
		p.mu.Unlock()
	}
	if changed {
		s.signal()
	}
	return result
}
