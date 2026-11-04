package evidence

import (
	"log"
	"os"
	"strings"
)

// CompatWindow is the consumer-side compatibility window for
// implementation.complete messages that carry no evidence record. It is read
// from EVIDENCE_COMPAT_WINDOW by every consumer of the topic through
// CompatWindowFromEnv, so the consumers cannot drift from one another.
type CompatWindow string

const (
	// CompatWindowLegacy (opt-in: EVIDENCE_COMPAT_WINDOW=legacy): a message
	// without an evidence field is decided by the legacy success boolean,
	// with a warning. Intended only while pre-evidence producers drain.
	CompatWindowLegacy CompatWindow = "legacy"
	// CompatWindowStrict (the default when the variable is unset): a
	// message without an evidence field blocks. Also
	// the fallback for an unrecognised value, so a typo can only narrow the
	// gate, never widen it.
	CompatWindowStrict CompatWindow = "strict"

	// CompatWindowEnv is the environment variable both consumers read.
	CompatWindowEnv = "EVIDENCE_COMPAT_WINDOW"
)

// ParseCompatWindow maps a raw EVIDENCE_COMPAT_WINDOW value to a window,
// case- and whitespace-insensitively. Unset (empty) is strict: every
// producer emits evidence, and the legacy window — which turned a
// not-run verification into a pass — is opt-in for an upgrade with
// pre-evidence messages still in flight. An unrecognised value is strict
// too, with ok=false so the caller can
// log it.
func ParseCompatWindow(v string) (window CompatWindow, ok bool) {
	switch CompatWindow(strings.ToLower(strings.TrimSpace(v))) {
	case "":
		return CompatWindowStrict, true
	case CompatWindowLegacy:
		return CompatWindowLegacy, true
	case CompatWindowStrict:
		return CompatWindowStrict, true
	}
	return CompatWindowStrict, false
}

// CompatWindowFromEnv reads EVIDENCE_COMPAT_WINDOW once at startup. An
// unrecognised value is treated as strict (messages without evidence block)
// and logged with the offending value so the operator can see the typo.
func CompatWindowFromEnv() CompatWindow {
	raw := os.Getenv(CompatWindowEnv)
	w, ok := ParseCompatWindow(raw)
	if !ok {
		log.Printf("evidence: %s=%q is not one of legacy|strict — treating as %s (messages without evidence block)", CompatWindowEnv, raw, w)
	}
	return w
}
