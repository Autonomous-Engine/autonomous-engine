package configstore

import "testing"

// F2: after a guarded *Tx write commits, the caller drops the local cache
// entry again so a stale value cannot outlive a lost config_changed.
func TestInvalidateCached_DropsTheEntry(t *testing.T) {
	cs := New(nil, nil)
	key := cacheKey("global", "default", "model_name", "llm")
	other := cacheKey("global", "default", "provider", "llm")
	cs.cache.Store(key, "old-model")
	cs.cache.Store(other, "anthropic")
	cs.InvalidateCached("global", "default", "model_name", "llm")
	if _, ok := cs.cache.Load(key); ok {
		t.Error("the entry is still cached")
	}
	if _, ok := cs.cache.Load(other); !ok {
		t.Error("an unrelated entry was dropped")
	}
}
