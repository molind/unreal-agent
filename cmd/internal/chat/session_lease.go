package chat

import "errors"

func (a *application) releaseSelection() error {
	if a.sessionRelease == nil {
		return nil
	}
	release := a.sessionRelease
	a.sessionRelease = nil
	return release()
}

// The caller has stopped/joined the old runtime. Retain its lease until the new
// selection is usable: a failed /resume must not let another host steal it.
func (a *application) switchSession(requested string) (failure string, result error) {
	oldID, oldRelease, oldDisplay := a.id, a.sessionRelease, *a.display
	changed := requested != string(a.id)
	if changed {
		id, release, err := openSession(a.ctx, a.store, requested)
		if err != nil {
			return "switch", err
		}
		a.id, a.sessionRelease = id, release
	}
	defer func() {
		if result != nil {
			// start() can launch a runtime and then fail to write its diagnostic.
			// Never release that runtime's lease until all effects have joined.
			result = errors.Join(result, a.stop())
			if changed {
				result = errors.Join(result, a.releaseSelection())
			}
			a.id, a.sessionRelease, *a.display = oldID, oldRelease, oldDisplay
		} else if changed && oldRelease != nil {
			result = oldRelease()
		}
	}()
	if err := a.replay(); err != nil {
		return "replay selected session", err
	}
	if err := a.announce(); err != nil {
		return "announce selected session", err
	}
	if err := a.start(); err != nil {
		return "start selected session", err
	}
	return "switch", nil
}
