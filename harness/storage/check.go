package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
)

func (db *DB) Check(ctx context.Context) error {
	rows, err := db.QueryContext(ctx, "PRAGMA integrity_check")
	if err != nil {
		return err
	}
	for rows.Next() {
		var result string
		if err = rows.Scan(&result); err != nil {
			break
		}
		if result != "ok" {
			err = fmt.Errorf("SQLite integrity: %s", result)
			break
		}
	}
	err = errors.Join(err, rows.Err(), rows.Close())
	if err != nil {
		return err
	}
	rows, err = db.QueryContext(ctx, "PRAGMA foreign_key_check")
	if err != nil {
		return err
	}
	if rows.Next() {
		_ = rows.Close()
		return errors.New("SQLite foreign key violation")
	}
	err = errors.Join(rows.Err(), rows.Close())
	if err != nil {
		return err
	}
	rows, err = db.QueryContext(ctx, "SELECT id FROM artifacts")
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			break
		}
		ids = append(ids, id)
	}
	err = errors.Join(err, rows.Err(), rows.Close())
	if err != nil {
		return err
	}
	for _, id := range ids {
		h := sha256.New()
		if err = db.Copy(ctx, id, h); err != nil {
			return err
		}
		if hex.EncodeToString(h.Sum(nil)) != id {
			return errors.New("artifact checksum mismatch")
		}
	}
	return nil
}
