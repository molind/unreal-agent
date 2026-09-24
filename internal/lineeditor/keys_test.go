package lineeditor

import (
	"bytes"
	"io"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestWordMotionKeySequences(t *testing.T) {
	for _, test := range []struct {
		sequence string
		key      rune
	}{
		{"\x1bb", keyAltLeft}, {"\x1bf", keyAltRight},
		{"\x1b[1;3D", keyAltLeft}, {"\x1b[1;3C", keyAltRight},
		{"\x1b[1;9D", keyAltLeft}, {"\x1b[1;9C", keyAltRight},
	} {
		t.Run(strings.ReplaceAll(test.sequence, "\x1b", "ESC"), func(t *testing.T) {
			key, rest := bytesToKey([]byte(test.sequence+"tail"), false)
			if key != test.key || string(rest) != "tail" {
				t.Fatalf("key/rest = %x %q, want %x tail", key, rest, test.key)
			}
			for size := 1; size < len(test.sequence); size++ {
				prefix := []byte(test.sequence[:size])
				key, rest := bytesToKey(prefix, false)
				if key != utf8.RuneError || !bytes.Equal(rest, prefix) {
					t.Fatalf("fragment was consumed prematurely: %q -> %x %q", prefix, key, rest)
				}
			}
			key, _ = bytesToKey([]byte(test.sequence), true)
			if key == keyAltLeft || key == keyAltRight {
				t.Fatal("word-motion key interpreted inside paste")
			}
		})
	}
}

// Deliver every escape/UTF-8 sequence in single-byte fragments, as a real PTY
// may. Output has a separate buffer so redraws cannot become keyboard input.
type fragmentedKeyWire struct {
	input string
	bytes.Buffer
}

func (w *fragmentedKeyWire) Read(p []byte) (int, error) {
	if len(w.input) == 0 {
		return 0, io.EOF
	}
	p[0] = w.input[0]
	w.input = w.input[1:]
	return 1, nil
}

func TestWordMotionEditingAcrossFragmentedInput(t *testing.T) {
	for _, keys := range [][2]string{{"\x1bb", "\x1bf"}, {"\x1b[1;3D", "\x1b[1;3C"}, {"\x1b[1;9D", "\x1b[1;9C"}} {
		w := &fragmentedKeyWire{input: "адзін два тры" + keys[0] + keys[0] + "X" + keys[1] + "Y\r"}
		term := NewTerminal(w, "you> ")
		line, err := term.ReadLine()
		if err != nil || line != "адзін Xдва Yтры" {
			t.Fatalf("word motion changed text/cursor: %q, %v", line, err)
		}
		w.input = keys[0] + keys[1] + "b f\x01" + keys[0] + "L\x05" + keys[1] + "R\r"
		line, err = term.ReadLine()
		if err != nil || line != "Lb fR" {
			t.Fatalf("boundary motion or ordinary b/f broken: %q, %v", line, err)
		}
	}
}
