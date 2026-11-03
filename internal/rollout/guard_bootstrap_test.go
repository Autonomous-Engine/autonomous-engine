package rollout

import (
	"context"
	"errors"
	"testing"

	"github.com/autonomousengine/autonomous-engine/internal/configsnap"
)

// changingSurface stands in for ProductionSurface: before and after differ.
func changingSurface(t *testing.T) {
	t.Helper()
	prev := captureSurface
	n := 0
	captureSurface = func(ctx context.Context, q configsnap.Querier) (Surface, error) {
		n++
		if n == 1 {
			return Surface{SurfaceKeyDefault: "cfg_before"}, nil
		}
		return Surface{SurfaceKeyDefault: "cfg_after"}, nil
	}
	t.Cleanup(func() { captureSurface = prev })
}

func bootstrapping(t *testing.T, v bool) {
	t.Helper()
	prev := isBootstrapping
	isBootstrapping = func(context.Context, configsnap.Querier) (bool, error) { return v, nil }
	t.Cleanup(func() { isBootstrapping = prev })
}

// A fresh install has no production behaviour to protect: the first LLM
// provider, model and embedding default were refused as "production
// configuration changes" with nothing configured (F10, F20).
func TestApplyGuardedAllowsFirstConfigDuringBootstrap(t *testing.T) {
	changingSurface(t)
	bootstrapping(t, true)
	tx := &guardTx{}
	res, err := ApplyGuarded(context.Background(), guardBeginner{tx}, DefaultChange{Action: "providers.set_default"})
	if err != nil {
		t.Fatalf("ApplyGuarded: %v", err)
	}
	if !res.Allowed || res.Reason != GuardBootstrap {
		t.Fatalf("res = %+v, want allowed with reason %q", res, GuardBootstrap)
	}
	if tx.commits != 1 || contains(tx.log, "INSERT INTO rollout_overrides") {
		t.Fatalf("commits=%d log=%q: bootstrap must commit without an override row", tx.commits, tx.log)
	}
}

func TestApplyGuardedStillRefusesOnceProductionExists(t *testing.T) {
	changingSurface(t)
	bootstrapping(t, false)
	tx := &guardTx{}
	_, err := ApplyGuarded(context.Background(), guardBeginner{tx}, DefaultChange{Action: "providers.set_default"})
	var refused *RefusedError
	if !errors.As(err, &refused) {
		t.Fatalf("err = %v, want RefusedError", err)
	}
	if tx.commits != 0 {
		t.Fatal("a refused change must not commit")
	}
}

// overlaySurface reflects the overlay's model into the surface id, so the
// before/after comparison sees exactly what the overlays say.
func overlaySurface(t *testing.T) {
	t.Helper()
	prev := captureSurface
	captureSurface = func(ctx context.Context, q configsnap.Querier) (Surface, error) {
		if o, ok := ctx.Value(overlayKey{}).(GlobalModelOverlay); ok && o.ModelID != nil {
			return Surface{SurfaceKeyDefault: "cfg_" + *o.ModelID}, nil
		}
		return Surface{SurfaceKeyDefault: "cfg_from_providers"}, nil
	}
	t.Cleanup(func() { captureSurface = prev })
}

// A credential write's "before" came from llm_providers while its "after"
// overlaid the credential value, so even re-saving the stored value was a
// change that needed an override (F10a). The before capture now overlays
// the stored value too.
func TestApplyGuarded_RewritingTheStoredCredentialIsUnchanged(t *testing.T) {
	overlaySurface(t)
	bootstrapping(t, false)
	m := "qwen3.6:35b"
	tx := &guardTx{}
	res, err := ApplyGuarded(context.Background(), guardBeginner{tx}, DefaultChange{
		Action: "credentials.create", BeforeOverlay: &GlobalModelOverlay{ModelID: &m}, Overlay: &GlobalModelOverlay{ModelID: &m},
	})
	if err != nil || res.Reason != GuardUnchanged {
		t.Fatalf("res=%+v err=%v, want unchanged", res, err)
	}

	n := "other"
	_, err = ApplyGuarded(context.Background(), guardBeginner{&guardTx{}}, DefaultChange{
		Action: "credentials.create", BeforeOverlay: &GlobalModelOverlay{ModelID: &m}, Overlay: &GlobalModelOverlay{ModelID: &n},
	})
	if !IsRefused(err) {
		t.Fatalf("a real change: err = %v, want refused", err)
	}
}
