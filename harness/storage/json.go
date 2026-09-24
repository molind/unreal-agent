package storage

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// Explicit JSON-pointer manifest avoids mistaking user-supplied {$blob: ...}
// objects for references. JSON numbers retain their exact decimal spelling.
type document struct {
	Value      any               `json:"value"`
	References map[string]string `json:"references,omitempty"`
}

func pointer(path, key string) string {
	return path + "/" + strings.ReplaceAll(strings.ReplaceAll(key, "~", "~0"), "/", "~1")
}
func walk(value any, path string, visit func(string, any) (any, error)) (any, error) {
	var err error
	switch v := value.(type) {
	case map[string]any:
		for k, x := range v {
			v[k], err = walk(x, pointer(path, k), visit)
			if err != nil {
				return nil, err
			}
		}
	case []any:
		for i, x := range v {
			v[i], err = walk(x, pointer(path, strconv.Itoa(i)), visit)
			if err != nil {
				return nil, err
			}
		}
	}
	return visit(path, value)
}
func PutJSON(ctx context.Context, tx *sql.Tx, data []byte) (string, error) {
	doc := document{References: map[string]string{}}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&doc.Value); err != nil {
		return "", err
	}
	value, err := walk(doc.Value, "", func(path string, value any) (any, error) {
		text, ok := value.(string)
		if !ok || len(text) < 4096 {
			return value, nil
		}
		id, _, err := PutTx(ctx, tx, strings.NewReader(text))
		if err != nil {
			return nil, err
		}
		doc.References[path] = id
		return nil, nil
	})
	if err != nil {
		return "", err
	}
	doc.Value = value
	encoded, err := json.Marshal(doc)
	if err != nil {
		return "", err
	}
	id, _, err := PutTx(ctx, tx, bytes.NewReader(encoded))
	return id, err
}
func (db *DB) JSON(ctx context.Context, id string) ([]byte, error) {
	data, err := db.Bytes(ctx, id)
	if err != nil {
		return nil, err
	}
	var doc document
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err = dec.Decode(&doc); err != nil {
		return nil, err
	}
	found := 0
	doc.Value, err = walk(doc.Value, "", func(path string, value any) (any, error) {
		ref, ok := doc.References[path]
		if !ok {
			return value, nil
		}
		if value != nil {
			return nil, fmt.Errorf("invalid JSON artifact manifest")
		}
		raw, err := db.Bytes(ctx, ref)
		if err != nil {
			return nil, err
		}
		found++
		return string(raw), nil
	})
	if err != nil {
		return nil, err
	}
	if found != len(doc.References) {
		return nil, fmt.Errorf("incomplete JSON artifact manifest")
	}
	return json.Marshal(doc.Value)
}
