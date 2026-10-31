// internal/codex/extractors/framework/registry.go
package framework

import "sort"

var registry = map[string]Extractor{}

func Register(e Extractor) { registry[e.Name()] = e }

func All() []Extractor {
	out := make([]Extractor, 0, len(registry))
	for _, e := range registry {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out
}

func Get(name string) (Extractor, bool) {
	e, ok := registry[name]
	return e, ok
}
