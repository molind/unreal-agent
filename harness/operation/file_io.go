package operation

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

type fileRevision struct {
	Version      int
	Path, SHA256 string
}
type fileReceipt struct {
	Version                  int
	InputHash, Before, After string
	Complete                 bool
	Result                   FileResult
}

func fileHash(data []byte) string { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }
func randomFileToken() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}
func boundedFileText(data []byte) (string, error) {
	if len(data) > MaxFileBytes {
		return "", fmt.Errorf("file exceeds %d MiB limit", MaxFileBytes>>20)
	}
	if !utf8.Valid(data) || strings.ContainsRune(string(data), 0) {
		return "", fmt.Errorf("file is not UTF-8 text (binary files are not supported)")
	}
	return string(data), nil
}
func lockFile(ctx context.Context, f *os.File) error {
	for {
		err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return nil
		}
		if err != unix.EWOULDBLOCK && err != unix.EAGAIN {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}
func openRegular(root *os.Root, name string, write bool) (*os.File, error) {
	mode := os.O_RDONLY
	if write {
		mode = os.O_RDWR
	}
	f, err := root.OpenFile(name, mode|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		_ = f.Close()
		if err == nil {
			err = fmt.Errorf("path is not a regular file")
		}
		return nil, err
	}
	return f, nil
}
func readRegular(f *os.File) ([]byte, error) {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(f, MaxFileBytes+1))
	if err != nil {
		return nil, err
	}
	_, err = boundedFileText(data)
	return data, err
}
func readPrivateJSON(root *os.Root, name string, value any) error {
	f, err := openRegular(root, name, false)
	if err != nil {
		return err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, (1<<20)+1))
	if err != nil {
		return err
	}
	if len(data) > 1<<20 {
		return fmt.Errorf("private file metadata exceeds limit")
	}
	return json.Unmarshal(data, value, json.RejectUnknownMembers(true))
}
func privateJSON(root *os.Root, name string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	tmp := ".receipt-" + randomFileToken()
	f, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer root.Remove(tmp)
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	err = errors.Join(err, f.Close())
	if err != nil {
		return err
	}
	if err = root.Rename(tmp, name); err != nil {
		return err
	}
	return syncFileRoot(root)
}
func syncFileRoot(root *os.Root) error {
	f, err := root.Open(".")
	if err != nil {
		return err
	}
	return errors.Join(f.Sync(), f.Close())
}
func bindRevision(root *os.Root, path, hash string) (string, error) {
	for range 8 {
		token := randomFileToken()
		f, err := root.OpenFile(token+".json", os.O_WRONLY|os.O_CREATE|os.O_EXCL|unix.O_NOFOLLOW, 0600)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		data, err := json.Marshal(fileRevision{Version: 1, Path: path, SHA256: hash})
		if err == nil {
			_, err = f.Write(data)
		}
		if err == nil {
			err = f.Sync()
		}
		err = errors.Join(err, f.Close())
		if err == nil {
			err = syncFileRoot(root)
		}
		return token, err
	}
	return "", fmt.Errorf("cannot allocate file revision")
}
func executeFile(ctx context.Context, id ID, in FileInput) (FileResult, error) {
	result := FileResult{Path: in.Path}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	// Split without filepath.Clean: preserve symlink/.. filesystem semantics.
	split := strings.LastIndexByte(in.Path, '/')
	parent, name := in.Path[:split+1], in.Path[split+1:]
	if name == "" || name == "." || name == ".." {
		return result, fmt.Errorf("path must name a file")
	}
	parent, err := filepath.EvalSymlinks(parent)
	if err != nil {
		return result, err
	}
	canonical := filepath.Join(parent, name)
	root, err := os.OpenRoot(parent)
	if err != nil {
		return result, err
	}
	defer root.Close()
	revisionsDir := filepath.Join(in.BaseDirectory, "revisions")
	if err := os.MkdirAll(revisionsDir, 0700); err != nil {
		return result, err
	}
	revisions, err := os.OpenRoot(revisionsDir)
	if err != nil {
		return result, err
	}
	defer revisions.Close()
	if in.Action == "Read" {
		f, err := openRegular(root, name, false)
		if err != nil {
			return result, err
		}
		defer f.Close()
		data, err := readRegular(f)
		if err != nil {
			return result, err
		}
		if err := ctx.Err(); err != nil {
			return result, err
		}
		result.Revision, err = bindRevision(revisions, canonical, fileHash(data))
		if err != nil {
			return result, err
		}
		lines := strings.SplitAfter(string(data), "\n")
		if len(lines) > 0 && lines[len(lines)-1] == "" {
			lines = lines[:len(lines)-1]
		}
		result.Offset, result.TotalLines = in.Offset, len(lines)
		start := min(in.Offset-1, len(lines))
		end := min(start+in.Limit, len(lines))
		var text strings.Builder
		for i := start; i < end; i++ {
			fmt.Fprintf(&text, "%d: %s", i+1, lines[i])
			if !strings.HasSuffix(lines[i], "\n") {
				text.WriteByte('\n')
			}
		}
		result.Text, result.Truncated = BoundOutput(text.String(), FilePreviewLimit)
		if end < len(lines) {
			result.NextOffset = end + 1
		}
		return result, nil
	}
	if string(id) == "" || strings.Trim(string(id), "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-") != "" {
		return result, fmt.Errorf("invalid file operation ID")
	}
	opDir := filepath.Join(in.BaseDirectory, string(id))
	if err := os.MkdirAll(opDir, 0700); err != nil {
		return result, err
	}
	journal, err := os.OpenRoot(opDir)
	if err != nil {
		return result, err
	}
	defer journal.Close()
	guard, err := journal.OpenFile("lock", os.O_RDWR|os.O_CREATE|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0600)
	if err != nil {
		return result, err
	}
	defer guard.Close()
	if err := lockFile(ctx, guard); err != nil {
		return result, err
	}
	encoded, _ := json.Marshal(in, json.Deterministic(true))
	inputHash := fileHash(encoded)
	var receipt fileReceipt
	err = readPrivateJSON(journal, "transaction.json", &receipt)
	if err == nil {
		if receipt.Version != 1 || receipt.InputHash != inputHash {
			return result, fmt.Errorf("file operation receipt does not match request")
		}
		if receipt.Complete {
			result = receipt.Result
			result.Recovered = true
			return result, nil
		}
		f, err := openRegular(root, name, false)
		if err == nil {
			data, readErr := readRegular(f)
			_ = f.Close()
			if readErr == nil && fileHash(data) == receipt.After {
				receipt.Complete = true
				receipt.Result.Applied = true
				receipt.Result.Recovered = true
				if err := privateJSON(journal, "transaction.json", receipt); err != nil {
					return result, err
				}
				return receipt.Result, nil
			}
		}
		return result, fmt.Errorf("interrupted file mutation has an uncertain outcome; no change repeated. Read the file and issue a new operation")
	}
	if !errors.Is(err, os.ErrNotExist) {
		return result, err
	}
	expected := "missing"
	if in.Revision != "missing" {
		var revision fileRevision
		if err := readPrivateJSON(revisions, in.Revision+".json", &revision); err != nil {
			return result, fmt.Errorf("unknown revision; Read the file first: %w", err)
		}
		if revision.Version != 1 || revision.Path != canonical || len(revision.SHA256) != 64 {
			return result, fmt.Errorf("revision belongs to a different file or is invalid; Read this file first")
		}
		expected = revision.SHA256
	}
	var before []byte
	mode := os.FileMode(0644)
	var original os.FileInfo
	f, err := openRegular(root, name, true)
	if err == nil {
		defer f.Close()
		if expected == "missing" {
			return result, fmt.Errorf("file already exists; Read it and provide its revision before overwriting")
		}
		if err := lockFile(ctx, f); err != nil {
			return result, err
		}
		original, err = f.Stat()
		if err != nil {
			return result, err
		}
		if original.Mode().Perm()&0222 == 0 {
			return result, fmt.Errorf("refusing to replace a read-only file")
		}
		var stat unix.Stat_t
		if err := unix.Fstat(int(f.Fd()), &stat); err != nil {
			return result, err
		}
		if stat.Nlink > 1 {
			return result, fmt.Errorf("refusing to replace a hard-linked file")
		}
		mode = original.Mode().Perm()
		before, err = readRegular(f)
		if err != nil {
			return result, err
		}
		if fileHash(before) != expected {
			return result, fmt.Errorf("file changed since Read; read again before editing")
		}
	} else if errors.Is(err, os.ErrNotExist) && expected == "missing" {
		result.Created = true
	} else {
		return result, err
	}
	after := in.Content
	if in.Action == "Edit" {
		result.Replacements = strings.Count(string(before), in.OldText)
		if result.Replacements == 0 {
			return result, fmt.Errorf("old_text was not found; no file changed")
		}
		if result.Replacements > 1 && !in.ReplaceAll {
			return result, fmt.Errorf("old_text matches %d places; provide unique context or replace_all=true", result.Replacements)
		}
		after = strings.ReplaceAll(string(before), in.OldText, in.NewText)
	}
	if _, err := boundedFileText([]byte(after)); err != nil {
		return result, err
	}
	if !result.Created && after == string(before) {
		return result, fmt.Errorf("replacement would not change the file")
	}
	result.Revision, err = bindRevision(revisions, canonical, fileHash([]byte(after)))
	if err != nil {
		return result, err
	}
	diff := fileDiff(in.Path, string(before), after, result.Created)
	result.Diff, result.Truncated = BoundOutput(diff, FilePreviewLimit)
	result.DiffPath = filepath.Join(opDir, "change.diff")
	capture, err := journal.OpenFile("change.diff", os.O_WRONLY|os.O_CREATE|os.O_EXCL|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return result, err
	}
	_, err = io.WriteString(capture, diff)
	if err == nil {
		err = capture.Sync()
	}
	err = errors.Join(err, capture.Close())
	if err != nil {
		return result, err
	}
	receipt = fileReceipt{Version: 1, InputHash: inputHash, Before: expected, After: fileHash([]byte(after)), Result: result}
	if err := privateJSON(journal, "transaction.json", receipt); err != nil {
		return result, err
	}
	tmp := ".unreal-edit-" + randomFileToken()
	staged, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return result, err
	}
	defer root.Remove(tmp)
	_, err = io.WriteString(staged, after)
	if err == nil {
		err = staged.Chmod(mode)
	}
	if err == nil {
		err = staged.Sync()
	}
	err = errors.Join(err, staged.Close())
	if err != nil {
		return result, err
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if !result.Created {
		info, err := root.Lstat(name)
		if err != nil || !os.SameFile(original, info) {
			return result, fmt.Errorf("file identity changed while editing; no replacement performed")
		}
		current, err := readRegular(f)
		if err != nil || fileHash(current) != expected {
			return result, fmt.Errorf("file contents changed while editing; no replacement performed")
		}
		if err := ctx.Err(); err != nil {
			return result, err
		}
		err = root.Rename(tmp, name)
	} else {
		err = root.Link(tmp, name)
	} // Atomic create-only, never overwrite a racing creator.
	if err != nil {
		return result, err
	}
	result.Applied = true
	receipt.Result = result
	// Once committed, finish the receipt even if cancellation arrived. Callers
	// may observe canceled status, but recovery must never blindly repeat the write.
	if err := syncFileRoot(root); err != nil {
		return result, fmt.Errorf("file replaced but directory sync failed; inspect before retrying: %w", err)
	}
	receipt.Complete = true
	if err := privateJSON(journal, "transaction.json", receipt); err != nil {
		return result, fmt.Errorf("file replaced but receipt failed; inspect before retrying: %w", err)
	}
	return result, nil
}
