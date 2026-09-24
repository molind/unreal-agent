package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/klauspost/compress/zstd"
	"golang.org/x/sys/unix"
)

const ChunkSize = 128 << 10
const MaxRead = 8 << 20

// Reference is a stable capture alias. It remains valid after the spool is
// removed or the database is exported. Artifact IDs themselves address content.
func Reference(path string) string {
	if strings.HasPrefix(path, "artifact:") || strings.HasPrefix(path, "capture:") {
		return path
	}
	return "capture:" + Hash([]byte(path))
}
func IsReference(path string) bool {
	return strings.HasPrefix(path, "artifact:") || strings.HasPrefix(path, "capture:")
}

// PutTx stores bounded, independently compressed blocks. The caller commits
// artifact visibility together with its referencing event/receipt.
func PutTx(ctx context.Context, tx *sql.Tx, r io.Reader) (string, int64, error) {
	enc, err := zstd.NewWriter(nil, zstd.WithEncoderConcurrency(1), zstd.WithEncoderLevel(zstd.SpeedFastest))
	if err != nil {
		return "", 0, err
	}
	defer enc.Close()
	type block struct {
		offset int64
		id     string
	}
	blocks := []block{}
	hasher := sha256.New()
	buf := make([]byte, ChunkSize)
	var total int64
	for {
		if err = ctx.Err(); err != nil {
			return "", 0, err
		}
		n, readErr := io.ReadFull(r, buf)
		if readErr != nil && readErr != io.EOF && readErr != io.ErrUnexpectedEOF {
			return "", 0, readErr
		}
		if n > 0 {
			raw := buf[:n]
			_, _ = hasher.Write(raw)
			id := Hash(raw)
			data := enc.EncodeAll(raw, nil)
			codec := "zstd"
			if len(data) >= len(raw) {
				data = raw
				codec = "raw"
			}
			if _, err = tx.ExecContext(ctx, "INSERT OR IGNORE INTO chunks VALUES(?,?,?,?)", id, codec, n, data); err != nil {
				return "", 0, err
			}
			blocks = append(blocks, block{total, id})
			total += int64(n)
		}
		if readErr != nil {
			break
		}
	}
	id := hex.EncodeToString(hasher.Sum(nil))
	if _, err = tx.ExecContext(ctx, "INSERT OR IGNORE INTO artifacts VALUES(?,?)", id, total); err != nil {
		return "", 0, err
	}
	for _, b := range blocks {
		if _, err = tx.ExecContext(ctx, "INSERT OR IGNORE INTO artifact_chunks VALUES(?,?,?)", id, b.offset, b.id); err != nil {
			return "", 0, err
		}
	}
	return id, total, nil
}
func (db *DB) Put(ctx context.Context, r io.Reader) (string, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	id, _, err := PutTx(ctx, tx, r)
	if err != nil {
		return "", err
	}
	return "artifact:" + id, tx.Commit()
}
func (db *DB) resolve(ctx context.Context, ref string) (string, error) {
	if strings.HasPrefix(ref, "capture:") {
		var id string
		err := db.QueryRowContext(ctx, "SELECT artifact FROM captures WHERE ref=?", ref).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			err = os.ErrNotExist
		}
		return id, err
	}
	id := strings.TrimPrefix(ref, "artifact:")
	if len(id) != 64 || strings.Trim(id, "0123456789abcdef") != "" {
		return "", fmt.Errorf("invalid artifact reference")
	}
	return id, nil
}

// Read returns raw bytes in a bounded range and the complete artifact size.
// Every touched chunk is verified, even when reading only its head or tail.
func (db *DB) Read(ctx context.Context, ref string, offset, count int64) ([]byte, int64, error) {
	if offset < 0 || count < 0 || count > MaxRead {
		return nil, 0, fmt.Errorf("artifact range must be nonnegative and at most %d bytes", MaxRead)
	}
	id, err := db.resolve(ctx, ref)
	if err != nil {
		return nil, 0, err
	}
	var size int64
	if err = db.QueryRowContext(ctx, "SELECT size FROM artifacts WHERE id=?", id).Scan(&size); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			err = os.ErrNotExist
		}
		return nil, 0, err
	}
	if offset >= size || count == 0 {
		return []byte{}, size, nil
	}
	count = min(count, size-offset)
	rows, err := db.QueryContext(ctx, `SELECT a.offset,c.id,c.codec,c.size,c.data FROM artifact_chunks a JOIN chunks c ON c.id=a.chunk WHERE a.artifact=? AND a.offset>=? AND a.offset<? ORDER BY a.offset`, id, offset/ChunkSize*ChunkSize, offset+count)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	dec, err := zstd.NewReader(nil, zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(4*ChunkSize))
	if err != nil {
		return nil, 0, err
	}
	defer dec.Close()
	result := make([]byte, 0, int(count))
	next := offset / ChunkSize * ChunkSize
	for rows.Next() {
		var start int64
		var hash, codec string
		var rawSize int
		var data []byte
		if err = rows.Scan(&start, &hash, &codec, &rawSize, &data); err != nil {
			return nil, 0, err
		}
		if start != next || rawSize < 1 || rawSize > ChunkSize {
			return nil, 0, errors.New("invalid artifact chunk layout")
		}
		switch codec {
		case "zstd":
			data, err = dec.DecodeAll(data, nil)
			if err != nil {
				return nil, 0, err
			}
		case "raw":
		default:
			return nil, 0, fmt.Errorf("unknown artifact codec %q", codec)
		}
		if len(data) != rawSize || Hash(data) != hash {
			return nil, 0, errors.New("artifact checksum mismatch")
		}
		from := max(offset-start, 0)
		to := min(offset+count-start, int64(len(data)))
		if from > to {
			return nil, 0, errors.New("invalid artifact range")
		}
		result = append(result, data[from:to]...)
		next += int64(rawSize)
	}
	if err = rows.Err(); err != nil {
		return nil, 0, err
	}
	if int64(len(result)) != count {
		return nil, 0, errors.New("incomplete artifact")
	}
	return result, size, nil
}
func (db *DB) Copy(ctx context.Context, ref string, w io.Writer) error {
	var offset int64
	for {
		data, size, err := db.Read(ctx, ref, offset, ChunkSize)
		if err != nil {
			return err
		}
		if len(data) > 0 {
			n, err := w.Write(data)
			if err != nil {
				return err
			}
			if n != len(data) {
				return io.ErrShortWrite
			}
			offset += int64(n)
		}
		if offset >= size {
			return nil
		}
	}
}

// Capture never deletes the source: the terminal operation checkpoint must be
// durable first. Repeated imports are idempotent and reject changed spools.
func (db *DB) Capture(ctx context.Context, path string) (string, error) {
	ref := Reference(path)
	f, err := os.OpenFile(path, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if errors.Is(err, os.ErrNotExist) {
		if _, e := db.resolve(ctx, ref); e == nil {
			return ref, nil
		}
		return "", err
	}
	if err != nil {
		return "", err
	}
	defer f.Close()
	before, err := f.Stat()
	if err != nil {
		return "", err
	}
	if !before.Mode().IsRegular() {
		return "", errors.New("capture is not a regular file")
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	id, size, err := PutTx(ctx, tx, f)
	if err != nil {
		return "", err
	}
	after, err := f.Stat()
	if err != nil {
		return "", err
	}
	if size != before.Size() || size != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		return "", errors.New("capture changed during import")
	}
	var existing string
	err = tx.QueryRowContext(ctx, "SELECT artifact FROM captures WHERE path=?", path).Scan(&existing)
	if err == nil && existing != id {
		return "", errors.New("capture changed after import")
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	if _, err = tx.ExecContext(ctx, "INSERT OR IGNORE INTO captures(path,ref,artifact,size) VALUES(?,?,?,?)", path, ref, id, size); err != nil {
		return "", err
	}
	return ref, tx.Commit()
}

// RetainCapture imports a legacy backup that cleanup must never unlink.
func (db *DB) RetainCapture(ctx context.Context, path string) error {
	if _, err := db.Capture(ctx, path); err != nil {
		return err
	}
	_, err := db.ExecContext(ctx, "UPDATE captures SET retained=1 WHERE path=?", path)
	return err
}

// RemoveCapture is called only after the referencing terminal checkpoint commits.
// A failed unlink is recoverable: repeat after restart. Never remove an unknown,
// replaced, or modified spool. The source is not trusted merely by its pathname.
func (db *DB) RemoveCapture(ctx context.Context, path string) error {
	var expected string
	var retained bool
	err := db.QueryRowContext(ctx, "SELECT artifact,retained FROM captures WHERE path=?", path).Scan(&expected, &retained)
	if err == nil && retained {
		return nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("capture is not regular")
	}
	h := sha256.New()
	if _, err = io.Copy(h, f); err != nil {
		return err
	}
	now, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !os.SameFile(info, now) || info.Size() != now.Size() || !info.ModTime().Equal(now.ModTime()) || hex.EncodeToString(h.Sum(nil)) != expected {
		return errors.New("refusing to remove changed capture")
	}
	if err = os.Remove(path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	err = errors.Join(dir.Sync(), dir.Close())
	// Remove just the empty operation directory, never a recursive tree.
	_ = os.Remove(filepath.Dir(path))
	return err
}

func (db *DB) Bytes(ctx context.Context, ref string) ([]byte, error) {
	var b bytes.Buffer
	if err := db.Copy(ctx, ref, &b); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}
