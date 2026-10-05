// Package snapshot defines immutable, versioned flag snapshots shared by the
// server cache and future SDK. Cache reads never renew authoritative freshness.
package snapshot

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"time"

	"switchyard/pkg/evaluation"
)

const Version = 1
const MaxBytes = 1 << 20
const RefreshInterval = 2 * time.Second
const MaxAge = 30 * time.Second

var ErrInvalid = errors.New("invalid flag snapshot")
var ErrExpired = errors.New("flag snapshot expired")

// Key never contains a user identity, custom attributes or a request result.
type Key struct {
	ProjectID     string
	EnvironmentID string
	FlagKey       string
}

type envelope struct {
	Version    int                   `json:"version"`
	VerifiedAt time.Time             `json:"verified_at"`
	Definition evaluation.Definition `json:"definition"`
}

// Snapshot owns all byte slices and its compiled definition. Sharing its pointer
// between concurrent readers is safe; updates replace the pointer as a whole.
type Snapshot struct {
	key        Key
	flagID     string
	revision   int64
	verifiedAt time.Time
	compiled   *evaluation.Compiled
	safe       evaluation.Value
	definition []byte
	wire       []byte
}

// New is for an authoritative database read. Capture verifiedAt before issuing
// that read, so pool wait/query time cannot make an old read seem newly verified.
func New(d evaluation.Definition, verifiedAt time.Time) (*Snapshot, error) {
	verifiedAt = verifiedAt.UTC()
	if verifiedAt.IsZero() || verifiedAt.Year() < 1970 || verifiedAt.Year() > 9999 ||
		len(d.ProjectID) > 128 || len(d.EnvironmentID) > 128 || len(d.FlagID) > 128 {
		return nil, ErrInvalid
	}
	compiled, err := evaluation.Compile(d)
	if err != nil {
		return nil, ErrInvalid
	}
	definition, err := compiled.MarshalJSON()
	if err != nil || len(definition) > MaxBytes {
		return nil, ErrInvalid
	}
	// Compile deep-copies input. Derive all other owned fields from that copy.
	var owned evaluation.Definition
	if json.Unmarshal(definition, &owned) != nil {
		return nil, ErrInvalid
	}
	wire, err := json.Marshal(envelope{Version, verifiedAt, owned})
	if err != nil || len(wire) > MaxBytes {
		return nil, ErrInvalid
	}
	return &Snapshot{key: Key{owned.ProjectID, owned.EnvironmentID, owned.Key},
		flagID: owned.FlagID, revision: owned.Revision, verifiedAt: verifiedAt,
		compiled: compiled, safe: owned.Safe, definition: definition, wire: wire}, nil
}

// Decode preserves the stored verification time, binds the payload to the
// requested scope and rejects unknown versions/fields/trailing data. It grants
// no freshness: callers must check Usable against their own clock.
func Decode(body []byte, key Key) (*Snapshot, error) {
	if len(body) == 0 || len(body) > MaxBytes {
		return nil, ErrInvalid
	}
	var e envelope
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&e) != nil || e.Version != Version {
		return nil, ErrInvalid
	}
	if decoder.Decode(new(any)) != io.EOF {
		return nil, ErrInvalid
	}
	s, err := New(e.Definition, e.VerifiedAt)
	if err != nil || s.key != key {
		return nil, ErrInvalid
	}
	return s, nil
}

func (s *Snapshot) Key() Key                     { return s.key }
func (s *Snapshot) Revision() int64              { return s.revision }
func (s *Snapshot) VerifiedAt() time.Time        { return s.verifiedAt }
func (s *Snapshot) FlagID() string               { return s.flagID }
func (s *Snapshot) MarshalJSON() ([]byte, error) { return bytes.Clone(s.wire), nil }
func (s *Snapshot) DefinitionJSON() []byte       { return bytes.Clone(s.definition) }

// Weight bounds admission by a conservative payload accounting estimate, not a
// promise about Go allocator RSS. The container remains the hard process limit.
func (s *Snapshot) Weight() int64 { return int64(len(s.wire))*8 + 2048 }
func (s *Snapshot) Safe() evaluation.Value {
	return evaluation.Value{Type: s.safe.Type, Data: bytes.Clone(s.safe.Data)}
}

// Remaining is deliberately conservative about clock skew. A timestamp ahead
// of the reader's clock is unusable until that clock reaches it; no future-time
// tolerance or new cache-receipt timestamp can extend the 30-second contract.
func (s *Snapshot) Remaining(now time.Time) time.Duration {
	if now.Before(s.verifiedAt) {
		return 0
	}
	remaining := MaxAge - now.Sub(s.verifiedAt)
	if remaining <= 0 {
		return 0
	}
	return remaining
}
func (s *Snapshot) Usable(now time.Time) bool { return s.Remaining(now) > 0 }
func (s *Snapshot) NeedsRefresh(now time.Time) bool {
	return !s.Usable(now) || now.Sub(s.verifiedAt) >= RefreshInterval
}
func (s *Snapshot) Evaluate(now time.Time, userID string, attributes map[string]json.RawMessage) (evaluation.Result, error) {
	if !s.Usable(now) {
		return evaluation.Result{}, ErrExpired
	}
	return s.compiled.Evaluate(userID, attributes)
}

// CanReplace prevents both revision regression and invented same-revision data.
// A fresh authoritative read may extend verification of an identical revision.
func (s *Snapshot) CanReplace(old *Snapshot) bool {
	if old == nil {
		return true
	}
	if s.key != old.key || s.flagID != old.flagID || s.revision < old.revision {
		return false
	}
	if s.revision > old.revision {
		return true
	}
	return bytes.Equal(s.definition, old.definition) && s.verifiedAt.After(old.verifiedAt)
}
