package snapshot

import (
	"log"
	"sync"

	"github.com/autonomousengine/autonomous-engine/internal/metrics"
	"github.com/google/uuid"
)

// unpinnedMetric is the metric NoteUnpinned increments, one series per
// service: ae_snapshot_unpinned_total{service="..."}.
const unpinnedMetric = "ae_snapshot_unpinned_total"

var (
	unpinnedMu       sync.Mutex
	unpinnedCounters = map[string]*metrics.Counter{}
	unpinnedSeen     = map[string]bool{}
)

// unpinnedSeenCap bounds the once-per-item memory; past it the set is reset
// (a long-running service may then log an item a second time, never more
// than once per reset).
const unpinnedSeenCap = 10000

// NoteUnpinned records that service read the pin of workID (for what, e.g.
// "repository 5034…" or "implementation.tasks") and found none: the item
// predates pinning (or its parent did), so the caller keeps today's
// branch-name behaviour. It logs once per (service, work item) and increments
// ae_snapshot_unpinned_total{service} on every call. Returns true when this
// call logged.
func NoteUnpinned(service string, workID uuid.UUID, what string) bool {
	unpinnedMu.Lock()
	c := unpinnedCounterLocked(service)
	key := service + "/" + workID.String()
	first := !unpinnedSeen[key]
	if first {
		if len(unpinnedSeen) >= unpinnedSeenCap {
			unpinnedSeen = map[string]bool{}
		}
		unpinnedSeen[key] = true
	}
	unpinnedMu.Unlock()

	c.Inc()
	if first {
		log.Printf("%s: work item %s has no pinned base SHA (%s) — legacy-unpinned, keeping branch-name behaviour", service, workID, what)
	}
	return first
}

// RegisterUnpinned creates service's ae_snapshot_unpinned_total series so it
// reads 0 from startup instead of being absent until the first legacy item.
// Each service that reads pins calls it once (from an init in its pins file).
func RegisterUnpinned(service string) {
	unpinnedMu.Lock()
	defer unpinnedMu.Unlock()
	unpinnedCounterLocked(service)
}

func unpinnedCounterLocked(service string) *metrics.Counter {
	c, ok := unpinnedCounters[service]
	if !ok {
		c = metrics.NewLabeledCounter(unpinnedMetric, "Reads of a work item's base SHA pin that found none (legacy-unpinned items)", "service", service)
		unpinnedCounters[service] = c
	}
	return c
}

// UnpinnedCount returns ae_snapshot_unpinned_total for service (tests).
func UnpinnedCount(service string) int64 {
	unpinnedMu.Lock()
	defer unpinnedMu.Unlock()
	if c, ok := unpinnedCounters[service]; ok {
		return c.Value()
	}
	return 0
}
