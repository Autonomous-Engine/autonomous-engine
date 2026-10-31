package contracts

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/autonomousengine/autonomous-engine/internal/codex/extractors/contracts/rest_openapi"
	"github.com/autonomousengine/autonomous-engine/internal/codex/extractors/framework"
)

// The rest checker. A REST contract is named "<METHOD> <path>" (the codex
// rest_openapi extractor's naming) or just "<path>". The producer defines it
// in an OpenAPI 3 / Swagger 2 JSON document (paths, methods, top-level
// response properties) and/or a route registration the rest_openapi
// extractor parses (Go HandleFunc/GET..., JS app.get/router.post on /api/
// paths). The checker diffs the producer's pinned base against its head:
//
//   - the route (path + method) disappeared from both sources → every
//     consumer line that calls the path is a mismatch;
//   - a top-level response property disappeared or changed type in the
//     OpenAPI schema → every consumer file that calls the path and reads that
//     property is a mismatch (first use per file).
//
// Without an OpenAPI JSON schema the response fields cannot be compared and
// the result is unchecked (the route-level check still runs). YAML specs are
// not parsed (no YAML parser in the module) and say so.

type restOp struct {
	method string
	path   string
	fields fieldSet // nil when the operation has no parseable response schema
	file   string
}

var restMethods = []string{"GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS"}

// parseRESTName splits "GET /api/users/{id}" into ("GET", "/api/users/{id}").
// A bare path has method "".
func parseRESTName(name string) (method, path string) {
	f := strings.Fields(name)
	switch {
	case len(f) >= 2 && isMethod(f[0]):
		return strings.ToUpper(f[0]), f[1]
	case len(f) >= 1:
		return "", f[len(f)-1]
	}
	return "", ""
}

func isMethod(s string) bool {
	u := strings.ToUpper(s)
	if u == "ANY" {
		return true
	}
	for _, m := range restMethods {
		if u == m {
			return true
		}
	}
	return false
}

// pathKey is the comparison form of a route path: parameters ({id}, :id)
// become {}, no trailing slash, no query.
func pathKey(p string) string {
	if i := strings.IndexAny(p, "?#"); i >= 0 {
		p = p[:i]
	}
	segs := strings.Split(strings.Trim(p, "/"), "/")
	for i, s := range segs {
		if (strings.HasPrefix(s, "{") && strings.HasSuffix(s, "}")) || strings.HasPrefix(s, ":") {
			segs[i] = "{}"
		}
	}
	return "/" + strings.Join(segs, "/")
}

func methodMatches(want, got string) bool {
	return want == "" || want == "ANY" || got == "ANY" || got == "" || strings.EqualFold(want, got)
}

// openAPISpecs parses every OpenAPI/Swagger JSON document under root into
// operations; yaml reports whether a YAML spec was seen (and not parsed).
func openAPISpecs(root string) (ops []restOp, yaml bool) {
	for _, rel := range walkFiles(root, map[string]bool{".json": true, ".yaml": true, ".yml": true}) {
		base := strings.ToLower(filepath.Base(rel))
		ext := filepath.Ext(base)
		if ext != ".json" {
			if strings.Contains(base, "openapi") || strings.Contains(base, "swagger") {
				yaml = true
			}
			continue
		}
		b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil || len(b) > maxFileBytes {
			continue
		}
		var doc map[string]any
		if json.Unmarshal(b, &doc) != nil {
			continue
		}
		if doc["openapi"] == nil && doc["swagger"] == nil {
			continue
		}
		paths, _ := doc["paths"].(map[string]any)
		for p, item := range paths {
			methods, _ := item.(map[string]any)
			for m, op := range methods {
				if !isMethod(m) {
					continue
				}
				o := restOp{method: strings.ToUpper(m), path: p, file: rel}
				if opm, ok := op.(map[string]any); ok {
					o.fields = responseFields(doc, opm)
				}
				ops = append(ops, o)
			}
		}
	}
	sort.Slice(ops, func(i, j int) bool {
		if ops[i].path != ops[j].path {
			return ops[i].path < ops[j].path
		}
		return ops[i].method < ops[j].method
	})
	return ops, yaml
}

// responseFields is the top-level property set of the operation's success
// response schema (the lowest 2xx, else "default"); nil when there is none.
func responseFields(doc, op map[string]any) fieldSet {
	responses, _ := op["responses"].(map[string]any)
	codes := make([]string, 0, len(responses))
	for c := range responses {
		codes = append(codes, c)
	}
	sort.Strings(codes)
	var resp map[string]any
	for _, c := range codes {
		if strings.HasPrefix(c, "2") {
			resp, _ = responses[c].(map[string]any)
			break
		}
	}
	if resp == nil {
		resp, _ = responses["default"].(map[string]any)
	}
	if resp == nil {
		return nil
	}
	resp = resolveRef(doc, resp)
	var schema map[string]any
	if content, ok := resp["content"].(map[string]any); ok {
		for _, mt := range []string{"application/json", "*/*"} {
			if c, ok := content[mt].(map[string]any); ok {
				schema, _ = c["schema"].(map[string]any)
				break
			}
		}
		if schema == nil {
			for _, c := range content {
				if cm, ok := c.(map[string]any); ok {
					schema, _ = cm["schema"].(map[string]any)
					break
				}
			}
		}
	} else {
		schema, _ = resp["schema"].(map[string]any) // Swagger 2
	}
	if schema == nil {
		return nil
	}
	schema = resolveRef(doc, schema)
	if t, _ := schema["type"].(string); t == "array" {
		if items, ok := schema["items"].(map[string]any); ok {
			schema = resolveRef(doc, items)
		}
	}
	props, ok := schema["properties"].(map[string]any)
	if !ok {
		return nil
	}
	out := fieldSet{}
	for name, p := range props {
		out[name] = schemaType(doc, p)
	}
	return out
}

// resolveRef follows a local "#/..." $ref (a few hops at most).
func resolveRef(doc, node map[string]any) map[string]any {
	for i := 0; i < 8; i++ {
		ref, ok := node["$ref"].(string)
		if !ok || !strings.HasPrefix(ref, "#/") {
			return node
		}
		var cur any = doc
		for _, part := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
			m, ok := cur.(map[string]any)
			if !ok {
				return node
			}
			cur = m[strings.ReplaceAll(strings.ReplaceAll(part, "~1", "/"), "~0", "~")]
		}
		next, ok := cur.(map[string]any)
		if !ok {
			return node
		}
		node = next
	}
	return node
}

func schemaType(doc map[string]any, p any) string {
	m, ok := p.(map[string]any)
	if !ok {
		return ""
	}
	if ref, ok := m["$ref"].(string); ok {
		return ref[strings.LastIndex(ref, "/")+1:]
	}
	t, _ := m["type"].(string)
	if t == "array" {
		if items, ok := m["items"].(map[string]any); ok {
			return "array<" + schemaType(doc, items) + ">"
		}
	}
	if f, ok := m["format"].(string); ok && t != "" {
		return t + "(" + f + ")"
	}
	return t
}

// routeTable is the rest_openapi extractor's route registrations under root.
func routeTable(ctx context.Context, root string) []restOp {
	producers, err := rest_openapi.New().ProducerExtract(ctx, &framework.ExtractionContext{RepoPath: root})
	if err != nil {
		return nil
	}
	out := make([]restOp, 0, len(producers))
	for _, p := range producers {
		m, _ := p.Signature["method"].(string)
		path, _ := p.Signature["path"].(string)
		out = append(out, restOp{method: m, path: path, file: p.File})
	}
	return out
}

// findOp returns the operations of ops matching (method, path).
func findOp(ops []restOp, method, path string) []restOp {
	key := pathKey(path)
	var out []restOp
	for _, o := range ops {
		if pathKey(o.path) == key && methodMatches(method, o.method) {
			out = append(out, o)
		}
	}
	return out
}

// pickSchema returns the first matching operation that has a response
// schema, preferring GET.
func pickSchema(ops []restOp) (restOp, bool) {
	for _, pass := range []bool{true, false} {
		for _, o := range ops {
			if o.fields != nil && (!pass || o.method == "GET") {
				return o, true
			}
		}
	}
	return restOp{}, false
}

// consumerPathPattern matches a consumer's call of path in a string or
// template literal: '/api/users/' + id, `/api/users/${id}`, "/api/users/42".
func consumerPathPattern(path string) *regexp.Regexp {
	if i := strings.IndexAny(path, "?#"); i >= 0 {
		path = path[:i]
	}
	segs := strings.Split(strings.Trim(path, "/"), "/")
	const param = `(?:\$\{[^}]*\}|:[A-Za-z_]\w*|\{[A-Za-z_]\w*\}|[A-Za-z0-9_\-.~%]+)`
	var b strings.Builder
	for i, s := range segs {
		isParam := (strings.HasPrefix(s, "{") && strings.HasSuffix(s, "}")) || strings.HasPrefix(s, ":")
		b.WriteString("/")
		if isParam {
			if i == len(segs)-1 {
				b.WriteString(param + "?")
			} else {
				b.WriteString(param)
			}
			continue
		}
		b.WriteString(regexp.QuoteMeta(s))
	}
	return regexp.MustCompile(`['"\x60]` + `(?:[a-z]+://[^/'"\x60]*)?(?:\$\{[^}]*\})?` + b.String() + `/?(?:['"\x60?#]|$)`)
}

func checkREST(ctx context.Context, res Result, producer, consumer Side) Result {
	method, path := parseRESTName(res.Contract.Name)
	if path == "" || !strings.HasPrefix(path, "/") {
		return unchecked(res, fmt.Sprintf("the REST contract name %q has no path", res.Contract.Name))
	}
	if producer.Base == "" {
		return unchecked(res, "the producer repository has no pinned base to diff its head against")
	}
	baseSpec, baseYAML := openAPISpecs(producer.Base)
	headSpec, headYAML := openAPISpecs(producer.Head)
	baseOps := findOp(baseSpec, method, path)
	headOps := findOp(headSpec, method, path)
	baseRoutes := findOp(routeTable(ctx, producer.Base), method, path)
	headRoutes := findOp(routeTable(ctx, producer.Head), method, path)

	inBase := len(baseOps) > 0 || len(baseRoutes) > 0
	inHead := len(headOps) > 0 || len(headRoutes) > 0
	if !inBase && !inHead {
		why := "the producer defines it neither in an OpenAPI JSON document nor in a route registration the checker parses"
		if baseYAML || headYAML {
			why += " (a YAML OpenAPI document is present; YAML is not parsed)"
		}
		return unchecked(res, why)
	}
	if !inBase {
		res.Details = append(res.Details, "the producer adds this route; nothing the consumer used was removed")
		return finish(res)
	}

	re := consumerPathPattern(path)
	var callers []sourceFile
	var callLines [][]int
	for _, f := range readSources(consumer.Head, consumerExts) {
		var hits []int
		for i, l := range f.lines {
			if re.MatchString(l) {
				hits = append(hits, i)
			}
		}
		if len(hits) > 0 {
			callers = append(callers, f)
			callLines = append(callLines, hits)
		}
	}
	// The codex saw the consumer in a specific file: check it directly even
	// when the call is built in a way the path pattern does not recognise.
	if f, line, ok := codexConsumerFile(consumer.Head, res.Contract); ok && !hasFile(callers, f.rel) {
		callers = append(callers, f)
		callLines = append(callLines, []int{line})
	}
	if len(callers) == 0 {
		// Not seeing the consumer's use is not evidence that it has none.
		return unchecked(res, fmt.Sprintf("no call of %s found in the consumer head, so its use of the contract cannot be checked", path))
	}

	where := "route registration"
	if len(baseOps) > 0 {
		where = baseOps[0].file
	} else if len(baseRoutes) > 0 {
		where = baseRoutes[0].file
	}
	label := strings.TrimSpace(method + " " + path)
	if !inHead {
		for i, f := range callers {
			for _, idx := range callLines[i] {
				if len(res.Mismatches) >= maxMismatches {
					break
				}
				res.Mismatches = append(res.Mismatches, Mismatch{
					ProducerChange: fmt.Sprintf("removed %s (it was defined in %s)", label, where),
					ConsumerRef:    location(f, idx),
				})
			}
		}
		return finish(res)
	}

	baseOp, baseHasSchema := pickSchema(baseOps)
	headOp, headHasSchema := pickSchema(headOps)
	if !baseHasSchema || !headHasSchema {
		why := "the route is still registered, but its response fields cannot be compared: no OpenAPI JSON response schema"
		switch {
		case baseHasSchema:
			why += " in the producer head"
		case headHasSchema:
			why += " in the producer base"
		default:
			why += " in the producer"
		}
		if baseYAML || headYAML {
			why += " (a YAML OpenAPI document is present; YAML is not parsed)"
		}
		return unchecked(res, why)
	}
	changes := fieldChanges(baseOp.fields, headOp.fields, label+" response", headOp.file)
	for _, field := range sortedKeys(changes) {
		for _, f := range callers {
			if idx, ok := findFieldUse(f, field); ok && len(res.Mismatches) < maxMismatches {
				res.Mismatches = append(res.Mismatches, Mismatch{ProducerChange: changes[field], ConsumerRef: location(f, idx)})
			}
		}
	}
	if len(res.Mismatches) == 0 && len(changes) > 0 {
		res.Details = append(res.Details, fmt.Sprintf("the producer changed %d response field(s) the consumer does not read", len(changes)))
	}
	return finish(res)
}
