// internal/codex/extractors/framework/router.go
package framework

type Destination int

const (
	DestDirect           Destination = iota
	DestCandidate
	DestCandidateLowConf
)

const (
	highConfidenceThreshold = 0.90
	lowConfidenceThreshold  = 0.70
)

func Route(b *Blob, kind Kind, trustedAutoindex bool) Destination {
	if b.Confidence < lowConfidenceThreshold {
		return DestCandidateLowConf
	}
	if kind == KindLLM || kind == KindHybrid {
		return DestCandidate
	}
	if b.Confidence >= highConfidenceThreshold && trustedAutoindex {
		return DestDirect
	}
	return DestCandidate
}
