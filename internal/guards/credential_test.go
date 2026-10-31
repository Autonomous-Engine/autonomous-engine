package guards

import (
	"testing"
	"time"

	"github.com/autonomousengine/autonomous-engine/internal/credmonitor"
)

func TestCredentialDecision_NoCredential(t *testing.T) {
	v := decideCredential(nil, time.Now())
	if v.Valid {
		t.Fatal("missing credential must block")
	}
	if len(v.Missing) == 0 || v.Missing[0] != "credential" {
		t.Fatalf("expected Missing=credential, got %v", v.Missing)
	}
}

func TestCredentialDecision_ValidStatus(t *testing.T) {
	c := &credSnapshot{Status: credmonitor.StatusValid}
	v := decideCredential(c, time.Now())
	if !v.Valid {
		t.Fatal("valid status must pass")
	}
}

func TestCredentialDecision_InvalidStatus(t *testing.T) {
	c := &credSnapshot{Status: credmonitor.StatusInvalid, LastError: "HTTP 401"}
	v := decideCredential(c, time.Now())
	if v.Valid {
		t.Fatal("invalid must block")
	}
}

func TestCredentialDecision_ExpiredByDate(t *testing.T) {
	past := time.Now().Add(-time.Hour)
	c := &credSnapshot{Status: credmonitor.StatusValid, ExpiresAt: &past}
	v := decideCredential(c, time.Now())
	if v.Valid {
		t.Fatal("expired by date must block regardless of probe status")
	}
}

func TestCredentialDecision_ExpiringSoonStillValid(t *testing.T) {
	soon := time.Now().Add(3 * 24 * time.Hour)
	c := &credSnapshot{Status: credmonitor.StatusValid, ExpiresAt: &soon}
	v := decideCredential(c, time.Now())
	if !v.Valid {
		t.Fatal("expiring-soon must still allow (banner only)")
	}
}

func TestCredentialDecision_UnknownAllows(t *testing.T) {
	// The synchronous probe in CheckGitHubCredential runs before we reach
	// decideCredential in the normal path, so a lingering 'unknown' status
	// here means "no signal to block on".
	c := &credSnapshot{Status: credmonitor.StatusUnknown}
	v := decideCredential(c, time.Now())
	if !v.Valid {
		t.Fatal("unknown should fall through to allow; caller probes synchronously")
	}
}
