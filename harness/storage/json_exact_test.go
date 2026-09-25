package storage

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestJSONExactRepresentationAndLegacyReader(t *testing.T) {
	db := testDB(t)
	large := strings.Repeat(`\u0061`, 1500)
	cases := []string{
		` { "z": {"b":2,"a":1}, "a":1.00, "n":-0, "escaped":"\u003c\/\u0061" } `,
		`{"z":"` + large + `","a":["` + large + `",{"parts":[{"ref":"not-an-internal-reference"}]}]}`,
		`"` + large + `"`,
		`{"` + strings.Repeat("key", 1500) + `":"value"}`,
		"\n[true,null,18446744073709551615,1.234567890123456789]\t",
	}
	for i, raw := range cases {
		tx, err := db.BeginTx(t.Context(), nil)
		if err != nil {
			t.Fatal(err)
		}
		ref, err := PutJSON(t.Context(), tx, []byte(raw))
		if err != nil {
			_ = tx.Rollback()
			t.Fatal(err)
		}
		if err = tx.Commit(); err != nil {
			t.Fatal(err)
		}
		got, exact, err := db.JSONExact(t.Context(), ref)
		if err != nil || !exact || !bytes.Equal(got, []byte(raw)) {
			t.Fatalf("case %d did not round-trip exactly: %v", i, err)
		}
	}
	// Version-1 envelopes are still decodable, but explicitly marked inexact.
	old := []byte(`{"value":{"z":null,"a":18446744073709551615},"references":{"/z":"PLACEHOLDER"}}`)
	ref, err := db.Put(t.Context(), strings.NewReader("old large value"))
	if err != nil {
		t.Fatal(err)
	}
	old = bytes.Replace(old, []byte("PLACEHOLDER"), []byte(ref), 1)
	oldRef, err := db.Put(t.Context(), bytes.NewReader(old))
	if err != nil {
		t.Fatal(err)
	}
	got, exact, err := db.JSONExact(t.Context(), oldRef)
	if err != nil || exact || !bytes.Contains(got, []byte(`"z":"old large value"`)) {
		t.Fatal("legacy reader failed", err)
	}
	future, err := db.Put(t.Context(), strings.NewReader(`{"version":999}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = db.JSONExact(t.Context(), future); err == nil {
		t.Fatal("accepted unsupported envelope")
	}
}

func TestJSONExactRejectsInvalidInputAndManifest(t *testing.T) {
	db := testDB(t)
	for _, raw := range []string{"", `{"a":1,"a":2}`, `"unterminated`, `true false`} {
		tx, err := db.BeginTx(t.Context(), nil)
		if err != nil {
			t.Fatal(err)
		}
		_, err = PutJSON(t.Context(), tx, []byte(raw))
		_ = tx.Rollback()
		if err == nil {
			t.Fatal("accepted invalid JSON")
		}
	}
	for _, doc := range []exactDocument{
		{Version: 2, Size: 4, Parts: []jsonPart{{Text: "null", Ref: "ambiguous"}}},
		{Version: 2, Size: 4, Parts: []jsonPart{{}}},
		{Version: 2, Size: 3, Parts: []jsonPart{{Text: "null"}}},
		{Version: 2, Size: 5, Parts: []jsonPart{{Text: "null"}}},
	} {
		encoded, err := json.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		ref, err := db.Put(t.Context(), bytes.NewReader(encoded))
		if err != nil {
			t.Fatal(err)
		}
		if _, err = db.JSON(t.Context(), ref); err == nil {
			t.Fatal("accepted invalid exact manifest")
		}
	}
}
