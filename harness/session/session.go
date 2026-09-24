// Package session defines durable session and turn identity.
package session

import "time"

type ID string

type TurnID string

type TurnType string

const (
	TurnRegular    TurnType = "regular"
	TurnCompaction TurnType = "compaction"
)

type Session struct {
	ID        ID
	CreatedAt time.Time
}

type Turn struct {
	ID             TurnID
	PreviousTurnID TurnID
	Type           TurnType
	Compaction     *ContextCompaction `json:",omitempty"`
}

// ContextCompaction describes the exact non-system prefix replaced by a
// successful compaction response. The append-only original history is retained.
// Hashing excludes the system prompt so updated workspace instructions can be
// used on resume. Unsupported versions/mismatches must fail, never guess.
type ContextCompaction struct {
	Version       int
	PrefixItems   int
	PrefixHash    string
	SummaryTokens int
}
