package configsnap

import (
	"encoding/json"
	"fmt"
	"sort"
)

// Change kinds.
const (
	ChangeAdded   = "added"
	ChangeRemoved = "removed"
	ChangeChanged = "changed"
)

// Change is one difference between two documents. Path uses dots for
// object keys and [i] for array indexes ("prompt_templates[3].content").
type Change struct {
	Path   string `json:"path"`
	Kind   string `json:"kind"`
	Before any    `json:"before,omitempty"`
	After  any    `json:"after,omitempty"`
}

// Diff returns the differences from a to b, ordered by path. Both
// documents are normalised through their canonical JSON first, so a
// document built in Go and the same document read back from JSONB compare
// equal. Arrays are compared index by index (the capture sorts every list
// by its identity columns, so the same row lands at the same index unless
// rows were added or removed before it).
func Diff(a, b Document) ([]Change, error) {
	na, err := normalise(a)
	if err != nil {
		return nil, err
	}
	nb, err := normalise(b)
	if err != nil {
		return nil, err
	}
	var out []Change
	diffValue("", na, nb, &out)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	if out == nil {
		out = []Change{}
	}
	return out, nil
}

func normalise(d Document) (any, error) {
	if d == nil {
		return nil, nil
	}
	c, err := Canonicalize(d)
	if err != nil {
		return nil, err
	}
	var v any
	if err := decodeJSON(c, &v); err != nil {
		return nil, fmt.Errorf("configsnap: diff: %w", err)
	}
	return v, nil
}

func diffValue(path string, a, b any, out *[]Change) {
	switch av := a.(type) {
	case map[string]any:
		bv, ok := b.(map[string]any)
		if !ok {
			*out = append(*out, Change{Path: path, Kind: ChangeChanged, Before: a, After: b})
			return
		}
		keys := map[string]struct{}{}
		for k := range av {
			keys[k] = struct{}{}
		}
		for k := range bv {
			keys[k] = struct{}{}
		}
		sorted := make([]string, 0, len(keys))
		for k := range keys {
			sorted = append(sorted, k)
		}
		sort.Strings(sorted)
		for _, k := range sorted {
			p := k
			if path != "" {
				p = path + "." + k
			}
			x, inA := av[k]
			y, inB := bv[k]
			switch {
			case inA && !inB:
				*out = append(*out, Change{Path: p, Kind: ChangeRemoved, Before: x})
			case !inA && inB:
				*out = append(*out, Change{Path: p, Kind: ChangeAdded, After: y})
			default:
				diffValue(p, x, y, out)
			}
		}
	case []any:
		bv, ok := b.([]any)
		if !ok {
			*out = append(*out, Change{Path: path, Kind: ChangeChanged, Before: a, After: b})
			return
		}
		n := len(av)
		if len(bv) > n {
			n = len(bv)
		}
		for i := 0; i < n; i++ {
			p := fmt.Sprintf("%s[%d]", path, i)
			switch {
			case i >= len(bv):
				*out = append(*out, Change{Path: p, Kind: ChangeRemoved, Before: av[i]})
			case i >= len(av):
				*out = append(*out, Change{Path: p, Kind: ChangeAdded, After: bv[i]})
			default:
				diffValue(p, av[i], bv[i], out)
			}
		}
	default:
		if !scalarEqual(a, b) {
			*out = append(*out, Change{Path: path, Kind: ChangeChanged, Before: a, After: b})
		}
	}
}

func scalarEqual(a, b any) bool {
	if an, ok := a.(json.Number); ok {
		bn, ok := b.(json.Number)
		return ok && an == bn
	}
	switch b.(type) {
	case map[string]any, []any:
		return false
	}
	return a == b
}
