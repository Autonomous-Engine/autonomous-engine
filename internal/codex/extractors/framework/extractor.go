// internal/codex/extractors/framework/extractor.go
package framework

import "context"

type Extractor interface {
	Name() string
	Family() string
	Kind() Kind
	CaresAbout(diff *GitDiff) bool
	ExtractFull(ctx context.Context, ec *ExtractionContext) ([]*Blob, error)
	ExtractIncremental(ctx context.Context, ec *ExtractionContext, prev []*Blob) ([]*Blob, error)
}

type ContractDetector interface {
	Extractor
	ProducerExtract(ctx context.Context, ec *ExtractionContext) ([]*ProducerContract, error)
	ConsumerScan(ctx context.Context, ec *ExtractionContext, producers []*ProducerContract) ([]*Blob, error)
}
