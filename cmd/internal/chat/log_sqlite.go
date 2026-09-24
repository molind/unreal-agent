package chat

import (
	"database/sql"
	"encoding/json/v2"
	"errors"
	"path/filepath"
	"time"
	"uuid"

	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/storage"
)

func openDatabaseLogs(db *storage.DB, safe func(string) string) *logs {
	return &logs{database: db, run: uuid.New().String(), directory: db.Path, diagnostic: db.Path + " (diagnostics table)", safe: safe, commands: map[string]logRecord{}}
}
func (l *logs) close() error {
	if l.file != nil {
		return l.file.Close()
	}
	return nil
}
func (l *logs) databaseCommand(id session.ID, op operation.Operation) error {
	if op.Status == operation.StatusReady {
		return nil
	}
	r := logRecord{Session: id, Operation: op.ID, Event: operationState(op)}
	switch op.Type {
	case operation.TypeShell:
		state, err := operation.DecodeShellState(op)
		if err != nil {
			return err
		}
		r.Stage = "command"
		r.Command = state.Input.Command
		r.Directory = state.Input.Directory
		r.Error = state.TerminalError
		if (state.Result != nil || state.CapturesClosed) && terminal(op.Status) {
			if state.OutPath != "" {
				r.Stdout = storage.Reference(state.OutPath)
			}
			if state.ErrPath != "" {
				r.Stderr = storage.Reference(state.ErrPath)
			}
		} else {
			r.Stdout = state.OutPath
			r.Stderr = state.ErrPath
		}
		if state.Result != nil {
			exit := state.Result.ExitCode
			r.ExitCode = &exit
		}
	case operation.TypeFile:
		state, err := operation.DecodeFileState(op)
		if err != nil {
			return err
		}
		r.Stage = "file"
		r.Command = state.Input.Action + ": " + state.Input.Path
		r.Directory = filepath.Dir(state.Input.Path)
		if state.Result != nil {
			r.Stdout = state.Result.DiffPath
			r.Error = state.Result.Error
		}
	default:
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	key := string(id) + "/" + string(op.ID)
	previous, known := l.commands[key]
	if !known {
		var data []byte
		err := l.database.QueryRow("SELECT payload FROM diagnostics WHERE session=? AND operation=? AND json_extract(payload,'$.stage') IN ('command','file') ORDER BY number DESC LIMIT 1", id, op.ID).Scan(&data)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil {
			if err = json.Unmarshal(data, &previous); err != nil {
				return err
			}
		}
	}
	if previous.Event == r.Event {
		return nil
	}
	r.Started = previous.Started
	if r.Started.IsZero() {
		r.Started = time.Now().UTC()
	}
	if terminal(op.Status) {
		r.Duration = time.Since(r.Started).Seconds()
	}
	if err := l.write(nil, r); err != nil {
		return err
	}
	l.commands[key] = r
	return nil
}
