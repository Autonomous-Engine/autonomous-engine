// internal/codex/extractors/framework/types.go
package framework

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"path/filepath"

	"github.com/google/uuid"
)

type Kind string

const (
	KindStatic Kind = "static"
	KindLLM    Kind = "llm"
	KindHybrid Kind = "hybrid"
)

type Evidence struct {
	File string `json:"file"`
	Line int    `json:"line,omitempty"`
	Note string `json:"note,omitempty"`
}

type Blob struct {
	Section    string     `json:"section"`
	Payload    []byte     `json:"payload"`
	Hash       string     `json:"hash,omitempty"`
	Confidence float64    `json:"confidence"`
	Evidence   []Evidence `json:"evidence,omitempty"`
}

type GitDiff struct {
	ParentSHA    string   `json:"parent_sha"`
	CurrentSHA   string   `json:"current_sha"`
	FilesChanged []string `json:"files_changed"`
	FilesAdded   []string `json:"files_added"`
	FilesRemoved []string `json:"files_removed"`
}

func (d *GitDiff) HasTouched(globs []string) bool {
	if d == nil {
		return false
	}
	all := append(append([]string{}, d.FilesChanged...), d.FilesAdded...)
	all = append(all, d.FilesRemoved...)
	for _, f := range all {
		for _, g := range globs {
			if ok, _ := filepath.Match(g, f); ok {
				return true
			}
			if len(g) > 3 && g[:3] == "**/" && len(f) >= len(g)-3 && f[len(f)-(len(g)-3):] == g[3:] {
				return true
			}
		}
	}
	return false
}

type GitStrategy struct {
	RepoID            uuid.UUID `json:"repo_id"`
	Pattern           string    `json:"pattern"`
	MainBranch        string    `json:"main_branch"`
	LongLivedBranches []string  `json:"long_lived_branches"`
	MergeStrategy     string    `json:"merge_strategy"`
	PRRequired        bool      `json:"pr_required"`
}

type ExtractionContext struct {
	RepoPath    string
	RepoID      uuid.UUID
	ProductID   uuid.UUID
	CommitSHA   string
	Branch      string
	ParentSHA   string
	Diff        *GitDiff
	WatchedSet  []string
	GitStrategy *GitStrategy
}

type ProducerContract struct {
	Type      string         `json:"type"`
	Name      string         `json:"name"`
	File      string         `json:"file"`
	Line      int            `json:"line,omitempty"`
	Signature map[string]any `json:"signature,omitempty"`
}

func CanonicalHash(payload []byte) string {
	var v any
	if err := json.Unmarshal(payload, &v); err != nil {
		sum := sha256.Sum256(payload)
		return hex.EncodeToString(sum[:])
	}
	canon, _ := canonicalJSON(v)
	sum := sha256.Sum256(canon)
	return hex.EncodeToString(sum[:])
}

func canonicalJSON(v any) ([]byte, error) {
	switch t := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		for i := 1; i < len(keys); i++ {
			for j := i; j > 0 && keys[j-1] > keys[j]; j-- {
				keys[j-1], keys[j] = keys[j], keys[j-1]
			}
		}
		buf := []byte{'{'}
		for i, k := range keys {
			if i > 0 {
				buf = append(buf, ',')
			}
			kb, _ := json.Marshal(k)
			buf = append(buf, kb...)
			buf = append(buf, ':')
			cv, _ := canonicalJSON(t[k])
			buf = append(buf, cv...)
		}
		buf = append(buf, '}')
		return buf, nil
	case []any:
		buf := []byte{'['}
		for i, x := range t {
			if i > 0 {
				buf = append(buf, ',')
			}
			cv, _ := canonicalJSON(x)
			buf = append(buf, cv...)
		}
		buf = append(buf, ']')
		return buf, nil
	default:
		return json.Marshal(t)
	}
}
