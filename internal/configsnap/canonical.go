// Package configsnap captures the configuration that shapes an agent run as
// an immutable, content-addressed snapshot (M4-05).
//
// A snapshot document holds the agent definition row, its prompts, the
// prompt templates in effect for the agent type, the resolved model and the
// LLM / embedding provider settings (secrets redacted), a codex_version
// fixed at 0 (the work item's codex is not configuration; migration 060),
// and the controller / checks / verifier versions. Its id is
// the lowercase hex sha256 of the document's canonical JSON, so two runs
// under the same configuration share one row and any change to any captured
// input yields a new id. Rows are inserted once and never updated.
package configsnap

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
)

// Canonicalize returns the canonical JSON encoding of v: objects with keys
// sorted bytewise, no insignificant whitespace, no HTML escaping, numbers
// kept exactly as written (json.Number). Two values that are equal as JSON
// documents canonicalize to the same bytes regardless of key order, Go map
// iteration order, or whether they came from Go structs or Postgres JSONB.
func Canonicalize(v any) ([]byte, error) {
	raw, err := marshalNoEscape(v)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var generic any
	if err := dec.Decode(&generic); err != nil {
		return nil, fmt.Errorf("configsnap: canonicalize: %w", err)
	}
	// encoding/json writes map keys in sorted order; re-encoding the generic
	// tree therefore fixes the key order at every level.
	return marshalNoEscape(generic)
}

func marshalNoEscape(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, fmt.Errorf("configsnap: encode: %w", err)
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// Hash returns the snapshot id of v (hex sha256 of its canonical JSON) and
// the canonical bytes.
func Hash(v any) (string, []byte, error) {
	c, err := Canonicalize(v)
	if err != nil {
		return "", nil, err
	}
	sum := sha256.Sum256(c)
	return hex.EncodeToString(sum[:]), c, nil
}

// idPattern is the shape of a snapshot id (migration 053's CHECK).
var idPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ValidID reports whether s has the shape of a snapshot id.
func ValidID(s string) bool { return idPattern.MatchString(s) }
