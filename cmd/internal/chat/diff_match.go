package chat

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/rivo/uniseg"
)

type diffRow struct {
	index  int
	text   string
	tokens []string
	counts map[string]int
	count  int
}

func newDiffRow(index int, text string) diffRow {
	row := diffRow{index: index, text: text, tokens: diffTokens(text), counts: make(map[string]int)}
	for _, token := range row.tokens {
		if strings.TrimSpace(token) != "" {
			row.counts[token]++
			row.count++
		}
	}
	return row
}

// Group identifiers/numbers and whitespace; keep punctuation separate without
// splitting a grapheme (combining accents, emoji modifiers, etc.). This is a
// language-neutral display tokenizer, not a syntax highlighter.
func diffTokens(value string) []string {
	var tokens []string
	start, previous := 0, -1
	g := uniseg.NewGraphemes(value)
	for g.Next() {
		pos, _ := g.Positions()
		r, _ := utf8.DecodeRuneInString(g.Str())
		class := 0
		if r == '_' || unicode.IsLetter(r) || unicode.IsNumber(r) || unicode.IsMark(r) {
			class = 1
		} else if unicode.IsSpace(r) {
			class = 2
		}
		if pos > start && (class == 0 || class != previous) {
			tokens = append(tokens, value[start:pos])
			start = pos
		}
		previous = class
	}
	if start < len(value) {
		tokens = append(tokens, value[start:])
	}
	return tokens
}

// Pair similar rows monotonically before looking for intraline edits. A newly
// inserted row must not steal matching letters/tokens from a later replacement.
// Similarity uses non-whitespace token overlap, so shared indentation alone is
// not a match. At least half the tokens must agree; unrelated rows stay uniform.
func diffRowSimilarity(a, b diffRow) uint32 {
	if a.text == b.text {
		return 1000
	}
	if a.count+b.count == 0 {
		return 0
	}
	common := 0
	for token, count := range a.counts {
		common += min(count, b.counts[token])
	}
	score := 2000 * common / (a.count + b.count)
	if score < 500 {
		return 0
	}
	return uint32(score)
}

func diffRowPairs(before, after []diffRow, budget *int) [][2]int {
	if len(before) == 0 || len(after) == 0 || len(before)+1 > *budget/(len(after)+1) {
		return nil
	}
	width := len(after) + 1
	cells := (len(before) + 1) * width
	// Bound both the alignment matrix and the total work hashing/comparing
	// row contents. Oversized blocks keep whole-row colors, not arbitrary
	// partial matches. Linear tokenization/rendering remains lossless.
	var oldBytes, newBytes int64
	for _, row := range before {
		oldBytes += int64(len(row.text))
	}
	for _, row := range after {
		newBytes += int64(len(row.text))
	}
	cost := int64(cells) + oldBytes*int64(len(after)) + newBytes*int64(len(before))
	if cost > int64(*budget) {
		return nil
	}
	*budget -= int(cost)
	scores, best := make([]uint16, cells), make([]uint32, cells)
	for i := len(before) - 1; i >= 0; i-- {
		for j := len(after) - 1; j >= 0; j-- {
			pos := i*width + j
			score := diffRowSimilarity(before[i], after[j])
			scores[pos] = uint16(score)
			best[pos] = max(best[pos+width], best[pos+1])
			if score > 0 {
				best[pos] = max(best[pos], score+best[pos+width+1])
			}
		}
	}
	var pairs [][2]int
	for i, j := 0, 0; i < len(before) && j < len(after); {
		pos := i*width + j
		score := uint32(scores[pos])
		if score > 0 && best[pos] == score+best[pos+width+1] {
			pairs = append(pairs, [2]int{i, j})
			i++
			j++
		} else if best[pos+width] >= best[pos+1] {
			i++
		} else {
			j++
		}
	}
	return pairs
}
