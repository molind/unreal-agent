package storage

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Version 1 decoded JSON into maps. Though semantically equivalent, re-encoding
// changes opaque provider JSON and invalidates byte-sensitive compaction hashes.
// Keep its decoder for existing databases; all new writes use exact spans.
type document struct {
	Value      any               `json:"value"`
	References map[string]string `json:"references,omitempty"`
}
type exactDocument struct {
	Version int        `json:"version"`
	Size    int        `json:"size"`
	Parts   []jsonPart `json:"parts"`
}
type jsonPart struct {
	Text string `json:"text,omitempty"`
	Ref  string `json:"ref,omitempty"`
}

// PutJSON preserves the complete input byte sequence, including object order,
// whitespace, numeric spelling and escapes. Large encoded string tokens are
// content-addressed independently. No application JSON is decoded/re-encoded.
func PutJSON(ctx context.Context, tx *sql.Tx, data []byte) (string, error) {
	if !jsontext.Value(data).IsValid() {
		return "", errors.New("invalid JSON document")
	}
	doc := exactDocument{Version: 2, Size: len(data)}
	literal := 0
	for i := 0; i < len(data); i++ {
		if data[i] != '"' {
			continue
		}
		start := i
		i++
		for data[i] != '"' {
			if data[i] == '\\' {
				i++
			}
			i++
		}
		// Validation above guarantees terminated strings and valid escapes.
		if i-start < 4096 {
			continue
		}
		if start > literal {
			doc.Parts = append(doc.Parts, jsonPart{Text: string(data[literal:start])})
		}
		ref, _, err := PutTx(ctx, tx, bytes.NewReader(data[start:i+1]))
		if err != nil {
			return "", err
		}
		doc.Parts = append(doc.Parts, jsonPart{Ref: ref})
		literal = i + 1
	}
	if literal < len(data) {
		doc.Parts = append(doc.Parts, jsonPart{Text: string(data[literal:])})
	}
	encoded, err := json.Marshal(doc)
	if err != nil {
		return "", err
	}
	id, _, err := PutTx(ctx, tx, bytes.NewReader(encoded))
	return id, err
}

func (db *DB) JSON(ctx context.Context, id string) ([]byte, error) {
	raw, _, err := db.JSONExact(ctx, id)
	return raw, err
}

// JSONExact reports whether the stored format preserves the writer's bytes.
// Legacy format-1 rows remain readable, but cannot recover their original order
// without an independently verified original journal.
func (db *DB) JSONExact(ctx context.Context, id string) ([]byte, bool, error) {
	data, err := db.Bytes(ctx, id)
	if err != nil {
		return nil, false, err
	}
	var header struct {
		Version int `json:"version"`
	}
	if err = json.Unmarshal(data, &header); err != nil {
		return nil, false, err
	}
	if header.Version == 0 {
		raw, err := db.legacyJSON(ctx, data)
		return raw, false, err
	}
	if header.Version != 2 {
		return nil, false, fmt.Errorf("unsupported JSON artifact format %d", header.Version)
	}
	var doc exactDocument
	if err = json.Unmarshal(data, &doc); err != nil {
		return nil, false, err
	}
	if doc.Size < 1 {
		return nil, false, errors.New("invalid JSON artifact size")
	}
	var out bytes.Buffer
	for _, part := range doc.Parts {
		if (part.Text == "") == (part.Ref == "") {
			return nil, false, errors.New("invalid JSON artifact span")
		}
		var raw []byte
		if part.Ref != "" {
			raw, err = db.Bytes(ctx, part.Ref)
			if err != nil {
				return nil, false, err
			}
		} else {
			raw = []byte(part.Text)
		}
		if len(raw) > doc.Size-out.Len() {
			return nil, false, errors.New("JSON artifact exceeds its recorded size")
		}
		_, _ = out.Write(raw)
	}
	if out.Len() != doc.Size || !jsontext.Value(out.Bytes()).IsValid() {
		return nil, false, errors.New("incomplete or invalid JSON artifact")
	}
	return out.Bytes(), true, nil
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
func (db *DB) legacyJSON(ctx context.Context, data []byte) ([]byte, error) {
	var doc document
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&doc); err != nil {
		return nil, err
	}
	found := 0
	value, err := walk(doc.Value, "", func(path string, value any) (any, error) {
		ref, ok := doc.References[path]
		if !ok {
			return value, nil
		}
		if value != nil {
			return nil, errors.New("invalid JSON artifact manifest")
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
		return nil, errors.New("incomplete JSON artifact manifest")
	}
	return json.Marshal(value)
}
