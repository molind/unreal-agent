package localfile

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	stdjson "encoding/json"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/storage"
	"golang.org/x/sys/unix"
)

const cleanupNamespace = "legacy-cleanup-v1"

type migrationFile struct {
	Path string
	Hash string
}
type migrationManifest struct {
	Version     int
	Verified    bool
	Files       []migrationFile
	Directories []string
}

// MigrateLegacy imports and verifies an idle source, then removes only the known
// files whose exact bytes are durably archived. The manifest survives partial
// cleanup, including deletion of the original session journals. Unknown files,
// skills, symlinks and the destination database are never recursively removed.
// The caller must hold the destination workspace writer lock.
func (s *Store) MigrateLegacy(ctx context.Context, directory string) error {
	if s.database == nil {
		return errors.New("migration requires SQLite destination")
	}
	source, err := filepath.Abs(directory)
	if err != nil {
		return err
	}
	var manifest migrationManifest
	encoded, err := s.database.GetMetadata(ctx, cleanupNamespace, source)
	known := err == nil
	if known {
		if err = json.Unmarshal(encoded, &manifest); err != nil {
			return err
		}
		if manifest.Version != 1 {
			return errors.New("unsupported legacy cleanup manifest")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	info, err := os.Lstat(source)
	if errors.Is(err, os.ErrNotExist) {
		if known && manifest.Verified {
			return pruneLegacyParents(source)
		}
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("legacy source is not a real directory: %s", source)
	}
	destinationInfo, err := os.Stat(s.directory)
	if err != nil {
		return err
	}
	inPlace := os.SameFile(info, destinationInfo)
	// Do not follow a project-local .harness symlink, even if its target is a
	// perfectly valid directory. System aliases such as macOS /var are allowed.
	if parent := filepath.Dir(source); filepath.Base(parent) == ".harness" {
		if info, err := os.Lstat(parent); err != nil {
			return err
		} else if !info.IsDir() {
			return errors.New("refusing symlinked .harness directory")
		}
	}
	files, directories, err := discoverMigrationFiles(source, manifest)
	if err != nil {
		return err
	}
	if !known && len(files) == 0 {
		entries, err := os.ReadDir(source)
		if err != nil {
			return err
		}
		if len(entries) != 0 {
			return nil
		}
	}
	lease, err := storage.LockLegacyDirectory(source, true)
	if err != nil {
		return err
	}
	defer lease.Close()
	if err = storage.LegacyIdle(ctx, source); err != nil {
		return err
	}
	root, err := os.OpenRoot(source)
	if err != nil {
		return err
	}
	defer root.Close()
	locked, err := lease.Stat()
	if err != nil {
		return err
	}
	opened, err := root.Stat(".")
	if err != nil {
		return err
	}
	if !os.SameFile(info, locked) || !os.SameFile(locked, opened) {
		return errors.New("legacy directory changed before migration")
	}
	// The initial discovery was only a cheap no-op check. Snapshot under the
	// source lease so a cooperating writer cannot add files between discovery
	// and import.
	files, directories, err = discoverMigrationFiles(source, manifest)
	if err != nil {
		return err
	}
	if known {
		for _, name := range files {
			if slices.ContainsFunc(manifest.Files, func(f migrationFile) bool { return f.Path == name }) {
				continue
			}
			// In-place destinations may contain new SQLite spools. Those are
			// not leftover legacy files and must not become cleanup targets.
			if manifest.Verified && inPlace && strings.HasPrefix(name, "operations"+string(filepath.Separator)) {
				continue
			}
			return fmt.Errorf("new legacy file appeared during migration: %s; source preserved", name)
		}
	}
	// Validate/import first, before freezing a cleanup snapshot. A corrupt
	// unimported journal can then be repaired and retried without a stale
	// snapshot preventing the retry. Verified manifests may have missing files.
	if !manifest.Verified {
		if err = s.ImportLegacy(ctx, source); err != nil {
			return err
		}
	}
	if !known {
		manifest = migrationManifest{Version: 1, Directories: directories}
		for _, name := range files {
			f, err := openMigrationFile(root, name)
			if err != nil {
				return err
			}
			hash, err := hashMigrationFile(f)
			_ = f.Close()
			if err != nil {
				return err
			}
			if err = s.database.RetainCapture(ctx, filepath.Join(source, name)); err != nil {
				return err
			}
			manifest.Files = append(manifest.Files, migrationFile{Path: name, Hash: hash})
		}
		if err = s.saveManifest(ctx, source, manifest); err != nil {
			return err
		}
	}
	if err = s.verifyMigration(ctx, source, root, manifest); err != nil {
		return err
	}
	if err = s.relocateLegacyOperations(ctx, source, manifest, inPlace); err != nil {
		return err
	}
	if err = storage.LegacyIdle(ctx, source); err != nil {
		return err
	}
	manifest.Verified = true
	if err = s.saveManifest(ctx, source, manifest); err != nil {
		return err
	}
	for _, file := range manifest.Files {
		if err = ctx.Err(); err != nil {
			return err
		}
		if err = removeMigrationFile(root, file); err != nil {
			return err
		}
	}
	// Children first; Remove (not RemoveAll) refuses nonempty directories. New
	// files, unknown data, skills and destination SQLite sidecars stay untouched.
	for _, dir := range manifest.Directories {
		if err = removeEmpty(root, dir); err != nil {
			return err
		}
	}
	named, err := os.Lstat(source)
	if err != nil {
		return err
	}
	if !os.SameFile(info, named) {
		return errors.New("legacy directory changed during cleanup")
	}
	return pruneLegacyParents(source)
}

func (s *Store) saveManifest(ctx context.Context, source string, manifest migrationManifest) error {
	data, err := json.Marshal(manifest)
	if err != nil {
		return err
	}
	return s.database.PutMetadata(ctx, cleanupNamespace, source, data)
}

func discoverMigrationFiles(source string, manifest migrationManifest) ([]string, []string, error) {
	entries, err := os.ReadDir(source)
	if err != nil {
		return nil, nil, err
	}
	sessions := map[string]bool{}
	for _, entry := range entries {
		if id, ok := strings.CutSuffix(entry.Name(), sessionFileSuffix); ok && validateSessionID(session.ID(id)) == nil {
			sessions[id] = true
		}
	}
	for _, file := range manifest.Files {
		if id, ok := strings.CutSuffix(file.Path, sessionFileSuffix); ok && filepath.Base(file.Path) == file.Path {
			sessions[id] = true
		}
	}
	var files []string
	dirs := map[string]bool{}
	err = filepath.WalkDir(source, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		parts := strings.Split(rel, string(filepath.Separator))
		if entry.IsDir() {
			// There is no reason to inspect unknown trees inside session storage.
			if parts[0] != "operations" && parts[0] != "logs" {
				return filepath.SkipDir
			}
			if parts[0] == "operations" && len(parts) > 1 && !sessions[parts[1]] {
				return filepath.SkipDir
			}
			// Only known structural directories qualify for empty pruning.
			if len(parts) == 1 || parts[0] == "operations" && len(parts) <= 3 || parts[0] == "logs" && parts[1] == "commands" && len(parts) <= 3 {
				dirs[rel] = true
			}
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 && (parts[0] == "operations" || parts[0] == "logs") {
			return fmt.Errorf("refusing symlinked legacy storage: %s", rel)
		}
		if !knownMigrationFile(parts, sessions) {
			return nil
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("refusing nonregular legacy file: %s", rel)
		}
		files = append(files, rel)
		for dir := filepath.Dir(rel); dir != "."; dir = filepath.Dir(dir) {
			dirs[dir] = true
		}
		return nil
	})
	var directories []string
	for dir := range dirs {
		directories = append(directories, dir)
	}
	slices.SortFunc(directories, func(a, b string) int { return len(b) - len(a) })
	return files, directories, err
}

func knownMigrationFile(p []string, sessions map[string]bool) bool {
	if len(p) == 1 {
		id, ok := strings.CutSuffix(p[0], sessionFileSuffix)
		return ok && sessions[id]
	}
	if p[0] == "logs" {
		return len(p) == 2 && strings.HasPrefix(p[1], "diagnostic-") && strings.HasSuffix(p[1], ".jsonl") ||
			len(p) == 4 && p[1] == "commands" && sessions[p[2]] && strings.HasSuffix(p[3], ".jsonl")
	}
	if len(p) != 4 || p[0] != "operations" || !sessions[p[1]] {
		return false
	}
	if p[2] == "revisions" {
		token, ok := strings.CutSuffix(p[3], ".json")
		return ok && len(token) == 16 && strings.Trim(token, "0123456789abcdef") == ""
	}
	return slices.Contains([]string{"out", "err", "change.diff", "transaction.json", "lock"}, p[3])
}

func openMigrationFile(root *os.Root, path string) (*os.File, error) {
	if !filepath.IsLocal(path) {
		return nil, errors.New("invalid migration path")
	}
	for prefix := filepath.Dir(path); prefix != "."; prefix = filepath.Dir(prefix) {
		info, err := root.Lstat(prefix)
		if err != nil {
			return nil, err
		}
		if !info.IsDir() {
			return nil, errors.New("refusing symlinked migration path")
		}
	}
	f, err := root.OpenFile(path, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		_ = f.Close()
		return nil, fmt.Errorf("not a regular migration file: %s", path)
	}
	return f, nil
}
func hashMigrationFile(f *os.File) (string, error) {
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func (s *Store) verifyMigration(ctx context.Context, source string, root *os.Root, manifest migrationManifest) error {
	for _, file := range manifest.Files {
		h := sha256.New()
		ref := storage.Reference(filepath.Join(source, file.Path))
		if err := s.database.Copy(ctx, ref, h); err != nil {
			return err
		}
		if hex.EncodeToString(h.Sum(nil)) != file.Hash {
			return fmt.Errorf("migration archive checksum mismatch: %s", file.Path)
		}
		if filepath.Base(file.Path) == file.Path && strings.HasSuffix(file.Path, sessionFileSuffix) {
			if err := s.verifyImportedJournal(ctx, source, file); err != nil {
				return err
			}
		}
		f, err := openMigrationFile(root, file.Path)
		if errors.Is(err, os.ErrNotExist) && manifest.Verified {
			continue
		}
		if err != nil {
			return err
		}
		hash, err := hashMigrationFile(f)
		_ = f.Close()
		if err != nil {
			return err
		}
		if hash != file.Hash {
			return fmt.Errorf("legacy file changed during migration: %s; source preserved", file.Path)
		}
	}
	return nil
}

func canonicalMigrationJSON(raw []byte) ([]byte, error) {
	var value any
	decoder := stdjson.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	return stdjson.Marshal(value)
}
func (s *Store) verifyImportedJournal(ctx context.Context, source string, file migrationFile) error {
	raw, err := s.database.Bytes(ctx, storage.Reference(filepath.Join(source, file.Path)))
	if err != nil {
		return err
	}
	state, committed, err := decodeLog(raw)
	if err != nil {
		return err
	}
	lines := bytes.Split(bytes.TrimSuffix(raw[:committed], []byte{'\n'}), []byte{'\n'})
	id := state.Snapshot.Session.ID
	var header string
	if err = s.database.QueryRowContext(ctx, "SELECT header FROM sessions WHERE id=?", id).Scan(&header); err != nil {
		return err
	}
	refs, err := s.sqlRefs(ctx, id, "SELECT payload FROM events WHERE session=? AND number<? ORDER BY number", id, len(lines))
	if err != nil {
		return err
	}
	refs = append([]string{header}, refs...)
	if len(refs) != len(lines) {
		return errors.New("imported history prefix is incomplete")
	}
	for i, ref := range refs {
		got, exact, err := s.database.JSONExact(ctx, ref)
		if err != nil {
			return err
		}
		if exact {
			if !bytes.Equal(got, lines[i]) {
				return fmt.Errorf("imported JSON representation differs at record %d", i)
			}
			continue
		}
		// Old format-1 imports normalized object order. Their raw archive is
		// retained and the read view can restore its bytes after this check.
		got, err = canonicalMigrationJSON(got)
		if err != nil {
			return err
		}
		want, err := canonicalMigrationJSON(lines[i])
		if err != nil {
			return err
		}
		if !bytes.Equal(got, want) {
			return fmt.Errorf("imported history differs at record %d", i)
		}
	}
	return nil
}

func removeMigrationFile(root *os.Root, file migrationFile) error {
	f, err := openMigrationFile(root, file.Path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	before, err := f.Stat()
	if err != nil {
		return err
	}
	hash, err := hashMigrationFile(f)
	if err != nil {
		return err
	}
	after, err := root.Lstat(file.Path)
	if err != nil {
		return err
	}
	if hash != file.Hash || !os.SameFile(before, after) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		return fmt.Errorf("legacy file changed before removal: %s; preserved", file.Path)
	}
	if err = root.Remove(file.Path); err != nil {
		return err
	}
	return syncMigrationDirectory(root, filepath.Dir(file.Path))
}
func syncMigrationDirectory(root *os.Root, path string) error {
	f, err := root.Open(path)
	if err != nil {
		return err
	}
	return errors.Join(f.Sync(), f.Close())
}
func removeEmpty(root *os.Root, path string) error {
	if !filepath.IsLocal(path) {
		return errors.New("invalid migration directory")
	}
	info, err := root.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return nil
	}
	if err = root.Remove(path); errors.Is(err, unix.ENOTEMPTY) || errors.Is(err, unix.EEXIST) {
		return nil
	}
	if err != nil {
		return err
	}
	return syncMigrationDirectory(root, filepath.Dir(path))
}
func pruneLegacyParents(source string) error {
	for _, path := range []string{source, filepath.Dir(source)} {
		if path != source && (filepath.Base(path) != ".harness" || filepath.Base(source) != "sessions") {
			break
		}
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return nil
		}
		if err = os.Remove(path); errors.Is(err, unix.ENOTEMPTY) || errors.Is(err, unix.EEXIST) {
			return nil
		}
		if err != nil {
			return err
		}
		parent, err := os.Open(filepath.Dir(path))
		if err != nil {
			return err
		}
		if err = errors.Join(parent.Sync(), parent.Close()); err != nil {
			return err
		}
	}
	return nil
}
