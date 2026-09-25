package operation

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/unreallabsai/unreal-agent/harness/storage"
	"golang.org/x/sys/unix"
)

type fileMetadata struct {
	db        *storage.DB
	base      string
	revisions *os.Root
}
type fileJournal struct {
	metadata *fileMetadata
	id       string
	root     *os.Root
	guard    *os.File
	unlock   func()
}

var fileOperationLocks [64]sync.Mutex

func openFileMetadata(db *storage.DB, base string) (*fileMetadata, error) {
	m := &fileMetadata{db: db, base: base}
	if db != nil {
		return m, nil
	}
	dir := filepath.Join(base, "revisions")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(dir)
	m.revisions = root
	return m, err
}
func (m *fileMetadata) close() {
	if m.revisions != nil {
		_ = m.revisions.Close()
	}
}
func (m *fileMetadata) bind(path, hash string) (string, error) {
	if m.db == nil {
		return bindRevision(m.revisions, path, hash)
	}
	data, err := json.Marshal(fileRevision{Version: 1, Path: path, SHA256: hash})
	if err != nil {
		return "", err
	}
	for range 8 {
		token := randomFileToken()
		result, err := m.db.Exec("INSERT OR IGNORE INTO metadata VALUES(?,?,?)", "revision:"+m.base, token, data)
		if err != nil {
			return "", err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return "", err
		}
		if n == 1 {
			return token, nil
		}
	}
	return "", errors.New("cannot allocate revision")
}
func (m *fileMetadata) readRevision(token string, value *fileRevision) error {
	if m.db == nil {
		return readPrivateJSON(m.revisions, token+".json", value)
	}
	data, err := m.db.GetMetadata(context.Background(), "revision:"+m.base, token)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, value, json.RejectUnknownMembers(true))
}
func (m *fileMetadata) journal(ctx context.Context, id string) (*fileJournal, error) {
	j := &fileJournal{metadata: m, id: id}
	if m.db != nil {
		// Hosts hold the session lease across all operations/recovery, excluding
		// the same receipt in another process. These stripes exclude duplicate
		// executions within that owner; target-file flock handles other sessions.
		hash := sha256.Sum256([]byte(m.base + "/" + id))
		mu := &fileOperationLocks[int(hash[0])%len(fileOperationLocks)]
		for !mu.TryLock() {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(10 * time.Millisecond):
			}
		}
		j.unlock = mu.Unlock
		return j, nil
	}
	dir := filepath.Join(m.base, id)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	j.root = root
	j.guard, err = root.OpenFile("lock", os.O_RDWR|os.O_CREATE|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0600)
	if err == nil {
		err = lockFile(ctx, j.guard)
	}
	if err != nil {
		j.close()
		return nil, err
	}
	return j, nil
}
func (j *fileJournal) close() {
	if j.unlock != nil {
		j.unlock()
	}
	if j.guard != nil {
		_ = j.guard.Close()
	}
	if j.root != nil {
		_ = j.root.Close()
	}
}
func (j *fileJournal) readReceipt(value *fileReceipt) error {
	if j.metadata.db == nil {
		return readPrivateJSON(j.root, "transaction.json", value)
	}
	data, err := j.metadata.db.GetMetadata(context.Background(), "receipt:"+j.metadata.base, j.id)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, value, json.RejectUnknownMembers(true))
}
func (j *fileJournal) writeReceipt(value fileReceipt) error {
	if j.metadata.db == nil {
		return privateJSON(j.root, "transaction.json", value)
	}
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	// A committed filesystem edit must finish its receipt despite cancellation.
	return j.metadata.db.PutMetadata(context.Background(), "receipt:"+j.metadata.base, j.id, data)
}
func (j *fileJournal) writeDiff(diff string) (string, error) {
	if j.metadata.db != nil {
		return j.metadata.db.Put(context.Background(), strings.NewReader(diff))
	}
	f, err := j.root.OpenFile("change.diff", os.O_WRONLY|os.O_CREATE|os.O_EXCL|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return "", err
	}
	_, err = io.WriteString(f, diff)
	if err == nil {
		err = f.Sync()
	}
	err = errors.Join(err, f.Close())
	return filepath.Join(j.metadata.base, j.id, "change.diff"), err
}
func readArtifact(ctx context.Context, in FileInput, db *storage.DB) (FileResult, error) {
	result := FileResult{Path: in.Path, Offset: in.Offset, Unit: "bytes"}
	if db == nil {
		return result, fmt.Errorf("artifact reading requires SQLite storage")
	}
	if in.Action != "Read" {
		return result, fmt.Errorf("artifacts are immutable")
	}
	data, size, err := db.Read(ctx, in.Path, int64(in.Offset-1), int64(in.Limit))
	if err != nil {
		return result, err
	}
	result.TotalBytes = size
	if utf8.Valid(data) {
		result.Text = string(data)
		result.Encoding = "utf-8"
	} else {
		result.Text = base64.StdEncoding.EncodeToString(data)
		result.Encoding = "base64"
	}
	next := int64(in.Offset) + int64(len(data))
	if next <= size {
		result.NextOffset = int(next)
	}
	return result, nil
}
