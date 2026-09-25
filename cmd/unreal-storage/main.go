// unreal-storage inspects, exports and migrates local workspace state without
// initializing an LLM provider or reading authentication configuration.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore/localfile"
	"github.com/unreallabsai/unreal-agent/harness/storage"
)

func main() {
	if err := run(context.Background(), os.Args[1:], os.Getenv, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run(ctx context.Context, args []string, getenv func(string) string, out, errout io.Writer) (result error) {
	flags := flag.NewFlagSet("unreal-storage", flag.ContinueOnError)
	flags.SetOutput(errout)
	workspace := flags.String("workspace", ".", "workspace identity")
	directory := flags.String("session-directory", "", "storage directory; default XDG workspace directory")
	offset := flags.Int64("offset", 0, "artifact byte offset, 0-based")
	count := flags.Int64("count", 128<<10, "maximum bytes for read (up to 8 MiB)")
	keepSource := flags.Bool("keep-source", false, "migrate: import without removing legacy files (offline copy mode)")
	flags.Usage = func() {
		fmt.Fprintln(errout, `Usage: unreal-storage [flags] command [arguments]
  path                      Print the database path without creating it
  sessions                  List saved sessions
  artifacts                 List capture references and artifact sizes
  read REF                  Write an artifact byte range to stdout
  export REF FILE           Export complete artifact; refuses overwrite
  history SESSION           Export complete canonical JSONL history to stdout
  logs [SESSION]            Export redacted diagnostic records as JSONL
  check                     Check SQLite structure, references and artifact bytes
  backup FILE               Consistent standalone database snapshot; no overwrite
  migrate LEGACY_DIRECTORY  Import, verify and clean idle legacy storage; --keep-source disables cleanup
Flags must precede the command.`)
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() < 1 {
		flags.Usage()
		return errors.New("command required")
	}
	work, err := filepath.Abs(*workspace)
	if err != nil {
		return err
	}
	dir := *directory
	if dir == "" {
		dir, err = storage.Directory(work, getenv)
	} else if !filepath.IsAbs(dir) {
		dir = filepath.Join(work, dir)
	}
	if err != nil {
		return err
	}
	command := flags.Arg(0)
	rest := flags.Args()[1:]
	arity := map[string]int{"path": 0, "sessions": 0, "artifacts": 0, "read": 1, "export": 2, "history": 1, "check": 0, "backup": 1, "migrate": 1}
	if command == "logs" {
		if len(rest) > 1 {
			return errors.New("logs takes at most one session ID")
		}
	} else if n, ok := arity[command]; !ok || len(rest) != n {
		return fmt.Errorf("unknown command or wrong argument count: %s", command)
	}
	if command == "path" {
		_, err = fmt.Fprintln(out, filepath.Join(dir, storage.Filename))
		return err
	}
	if command != "migrate" {
		if _, err = os.Stat(filepath.Join(dir, storage.Filename)); err != nil {
			return err
		}
	}
	if command == "migrate" {
		startup, err := storage.LockStartup(ctx, dir)
		if err != nil {
			return err
		}
		defer startup()
	}
	store, err := localfile.NewSQLite(dir)
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, store.Close()) }()
	db := store.Database()
	switch command {
	case "migrate":
		release, err := db.LockWriter()
		if err != nil {
			return err
		}
		defer release()
		if err = db.BindWorkspace(ctx, work); err != nil {
			return err
		}
		if *keepSource {
			lease, err := storage.LockLegacyDirectory(rest[0], true)
			if err != nil {
				return err
			}
			defer lease.Close()
			if err = storage.LegacyIdle(ctx, rest[0]); err != nil {
				return err
			}
			if err = store.ImportLegacy(ctx, rest[0]); err != nil {
				return err
			}
			_, err = fmt.Fprintln(out, "Migration verified; source files retained. Database:", db.Path)
			return err
		}
		if err = store.MigrateLegacy(ctx, rest[0]); err != nil {
			return err
		}
		_, err = fmt.Fprintln(out, "Migration verified; migrated source files removed, unknown files preserved. Database:", db.Path)
		return err
	case "sessions":
		list, err := store.ListSessions(ctx)
		if err != nil {
			return err
		}
		for _, s := range list {
			if _, err = fmt.Fprintln(out, s.ID, s.LastUpdatedAt); err != nil {
				return err
			}
		}
		return nil
	case "artifacts":
		rows, err := db.QueryContext(ctx, "SELECT ref,size FROM captures UNION ALL SELECT 'artifact:'||id,size FROM artifacts ORDER BY 1")
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var ref string
			var size int64
			if err = rows.Scan(&ref, &size); err != nil {
				return err
			}
			if _, err = fmt.Fprintln(out, ref, size); err != nil {
				return err
			}
		}
		return rows.Err()
	case "read":
		data, _, err := db.Read(ctx, rest[0], *offset, *count)
		if err != nil {
			return err
		}
		_, err = out.Write(data)
		return err
	case "export":
		f, err := os.OpenFile(rest[1], os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		err = db.Copy(ctx, rest[0], f)
		if err == nil {
			err = f.Sync()
		}
		err = errors.Join(err, f.Close())
		if err != nil {
			_ = os.Remove(rest[1])
		}
		return err
	case "history":
		return store.ExportJSONL(ctx, session.ID(rest[0]), out)
	case "logs":
		query := "SELECT payload FROM diagnostics"
		var params []any
		if len(rest) > 0 {
			query += " WHERE session=?"
			params = append(params, rest[0])
		}
		query += " ORDER BY number"
		rows, err := db.QueryContext(ctx, query, params...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var data []byte
			if err = rows.Scan(&data); err != nil {
				return err
			}
			if _, err = out.Write(append(data, '\n')); err != nil {
				return err
			}
		}
		return rows.Err()
	case "backup":
		return db.Backup(ctx, rest[0])
	case "check":
		if err = db.Check(ctx); err != nil {
			return err
		}
		_, err = fmt.Fprintln(out, "ok")
		return err
	}
	return nil
}
