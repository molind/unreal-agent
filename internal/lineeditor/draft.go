package lineeditor

// ClearDraft discards editable input and partial key sequences without adding
// them to history. Submitted history and an open selection are retained. Call
// between ReadLine calls; the editor lock still serializes asynchronous output.
func (t *Terminal) ClearDraft() (bool, error) {
	t.lock.Lock()
	defer t.lock.Unlock()
	cleared := len(t.line) != 0 || len(t.remainder) != 0
	t.line = t.line[:0]
	t.pos = 0
	t.draftTop = 0
	t.remainder = nil
	t.historyIndex = -1
	t.historyPending = ""
	if !cleared {
		return false, nil
	}
	t.repaint(t.statusRows(0))
	_, err := t.c.Write(t.outBuf)
	t.outBuf = t.outBuf[:0]
	return true, err
}
