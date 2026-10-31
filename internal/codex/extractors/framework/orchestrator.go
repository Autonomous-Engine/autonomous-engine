// internal/codex/extractors/framework/orchestrator.go
package framework

import (
	"context"
	"fmt"
	"sync"

	"github.com/jackc/pgx/v5/pgxpool"
)

type Registry interface {
	All() []Extractor
}

type globalRegistry struct{}

func (globalRegistry) All() []Extractor { return All() }

func GlobalRegistry() Registry { return globalRegistry{} }

func NewTestRegistry(es ...Extractor) Registry { return &testReg{es: es} }

type testReg struct{ es []Extractor }

func (t *testReg) All() []Extractor { return t.es }

type Orchestrator struct {
	db        *pgxpool.Pool
	blobs     *BlobStore
	snapshots *SnapshotStore
	failures  *FailureStore
	registry  Registry
}

func NewOrchestrator(
	db *pgxpool.Pool,
	blobs *BlobStore,
	snapshots *SnapshotStore,
	failures *FailureStore,
	registry Registry,
) *Orchestrator {
	return &Orchestrator{db: db, blobs: blobs, snapshots: snapshots, failures: failures, registry: registry}
}

func (o *Orchestrator) RunForCommit(ctx context.Context, ec *ExtractionContext) (*Snapshot, error) {
	var parentBlobs map[string][]*Blob
	if ec.ParentSHA != "" && o.snapshots != nil {
		parent, err := o.snapshots.Get(ctx, ec.RepoID, ec.ParentSHA)
		if err == nil && parent != nil && o.blobs != nil {
			resolved, extractorMap, _ := o.blobs.GetMany(ctx, parent.BlobHashes)
			parentBlobs = groupByExtractor(resolved, extractorMap)
		}
	}

	extractors := o.registry.All()
	var mu sync.Mutex
	allBlobHashes := []string{}

	for _, e := range extractors {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		if ec.Diff != nil && !e.CaresAbout(ec.Diff) {
			if prev, ok := parentBlobs[e.Name()]; ok {
				for _, b := range prev {
					mu.Lock()
					allBlobHashes = append(allBlobHashes, b.Hash)
					mu.Unlock()
				}
			}
			continue
		}

		blobs, err := safeExtract(ctx, e, ec, parentBlobs[e.Name()])
		if err != nil {
			if o.failures != nil {
				_ = o.failures.Record(ctx, ec.RepoID, ec.CommitSHA, e.Name(), err.Error())
			}
			for _, b := range parentBlobs[e.Name()] {
				mu.Lock()
				allBlobHashes = append(allBlobHashes, b.Hash)
				mu.Unlock()
			}
			continue
		}

		for _, b := range blobs {
			if o.blobs != nil {
				if err := o.blobs.Insert(ctx, b, e.Name()); err != nil {
					return nil, fmt.Errorf("blob insert for %s: %w", e.Name(), err)
				}
			} else {
				b.Hash = CanonicalHash(b.Payload)
			}
			mu.Lock()
			allBlobHashes = append(allBlobHashes, b.Hash)
			mu.Unlock()
		}
	}

	sn := &Snapshot{
		RepoID:     ec.RepoID,
		CommitSHA:  ec.CommitSHA,
		Branch:     ec.Branch,
		ParentSHA:  ec.ParentSHA,
		BlobHashes: allBlobHashes,
	}
	if o.snapshots != nil {
		if err := o.snapshots.Put(ctx, sn); err != nil {
			return nil, fmt.Errorf("snapshot put: %w", err)
		}
	}
	return sn, nil
}

func safeExtract(ctx context.Context, e Extractor, ec *ExtractionContext, prev []*Blob) (out []*Blob, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("extractor %s panic: %v", e.Name(), r)
		}
	}()
	if ec.ParentSHA == "" {
		return e.ExtractFull(ctx, ec)
	}
	return e.ExtractIncremental(ctx, ec, prev)
}

func groupByExtractor(blobs []*Blob, extractorMap map[string]string) map[string][]*Blob {
	out := map[string][]*Blob{}
	for _, b := range blobs {
		name := extractorMap[b.Hash]
		if name == "" {
			name = b.Section
		}
		out[name] = append(out[name], b)
	}
	return out
}
