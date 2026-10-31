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

	"github.com/autonomousengine/autonomous-engine/internal/codex/extractors/contracts/events"
	"github.com/autonomousengine/autonomous-engine/internal/codex/extractors/framework"
)

// The event checker. An event contract is named by its topic constant
// (TopicUserCreated, the codex events extractor's naming) or its topic value
// (user.created); the extractor's constants map one to the other. The
// payload is found by name: a JSON Schema document whose title or file name
// is the event's name, a Go struct or a TypeScript interface/type whose name
// is the event's name with an optional Payload/Message/Event/Data suffix
// (UserCreatedPayload). The checker diffs the payload's fields (JSON names:
// the json tag, else the Go field name) between the producer's base and
// head; a removed, renamed or retyped field is a mismatch wherever the
// consumer decodes it: a consumer file that references the event (constant
// or topic string) or defines a payload type of the same name, and uses the
// field (json tag, property access, quoted key).

// eventAliases is every name the contract goes by: its own name and, via the
// extractor's topic constants in the producer trees, the constant's value or
// the constant naming that value.
func eventAliases(ctx context.Context, name string, roots ...string) []string {
	set := map[string]bool{name: true}
	for _, root := range roots {
		if root == "" {
			continue
		}
		producers, err := events.New().ProducerExtract(ctx, &framework.ExtractionContext{RepoPath: root})
		if err != nil {
			continue
		}
		for _, p := range producers {
			v, _ := p.Signature["value"].(string)
			if p.Name == name && v != "" {
				set[v] = true
			}
			if v == name && p.Name != "" {
				set[p.Name] = true
			}
		}
	}
	out := make([]string, 0, len(set))
	for a := range set {
		out = append(out, a)
	}
	sort.Strings(out)
	return out
}

func aliasKeys(aliases []string) map[string]bool {
	keys := map[string]bool{}
	for _, a := range aliases {
		if k := normalizeName(a); k != "" {
			keys[k] = true
		}
	}
	return keys
}

var (
	goStructRe  = regexp.MustCompile(`^\s*type\s+(\w+)\s+struct\s*\{`)
	goFieldRe   = regexp.MustCompile("^\\s*([A-Z]\\w*)\\s+([^\\s`]+)\\s*(?:`([^`]*)`)?")
	jsonTagRe   = regexp.MustCompile(`json:"([^",]*)`)
	tsTypeRe    = regexp.MustCompile(`^\s*(?:export\s+)?(?:interface\s+(\w+)\b[^{]*\{|type\s+(\w+)\s*=\s*\{)`)
	tsFieldRe   = regexp.MustCompile(`^\s*(?:readonly\s+)?['"]?([A-Za-z_$][\w$]*)['"]?\??\s*:\s*([^;,]+)`)
	closeBodyRe = regexp.MustCompile(`^\s*\}`)
)

// payload is one payload definition found in a tree.
type payload struct {
	name   string
	file   string
	fields fieldSet
}

// findPayloads returns every payload definition under root whose name
// matches one of keys (normalizeName form).
func findPayloads(root string, keys map[string]bool) []payload {
	var out []payload
	for _, f := range readSources(root, map[string]bool{".go": true, ".ts": true, ".tsx": true, ".json": true}) {
		switch strings.ToLower(filepath.Ext(f.rel)) {
		case ".go":
			if strings.HasSuffix(f.rel, "_test.go") {
				continue
			}
			out = append(out, goPayloads(f, keys)...)
		case ".ts", ".tsx":
			out = append(out, tsPayloads(f, keys)...)
		case ".json":
			if p, ok := jsonSchemaPayload(root, f, keys); ok {
				out = append(out, p)
			}
		}
	}
	return out
}

func goPayloads(f sourceFile, keys map[string]bool) []payload {
	var out []payload
	for i := 0; i < len(f.lines); i++ {
		m := goStructRe.FindStringSubmatch(f.lines[i])
		if m == nil || !keys[normalizeName(m[1])] {
			continue
		}
		p := payload{name: m[1], file: f.rel, fields: fieldSet{}}
		for j := i + 1; j < len(f.lines) && !closeBodyRe.MatchString(f.lines[j]); j++ {
			fm := goFieldRe.FindStringSubmatch(f.lines[j])
			if fm == nil {
				continue
			}
			name := fm[1]
			if t := jsonTagRe.FindStringSubmatch(fm[3]); t != nil {
				if t[1] == "-" {
					continue
				}
				if t[1] != "" {
					name = t[1]
				}
			}
			p.fields[name] = fm[2]
		}
		out = append(out, p)
	}
	return out
}

func tsPayloads(f sourceFile, keys map[string]bool) []payload {
	var out []payload
	for i := 0; i < len(f.lines); i++ {
		m := tsTypeRe.FindStringSubmatch(f.lines[i])
		if m == nil {
			continue
		}
		name := m[1]
		if name == "" {
			name = m[2]
		}
		if !keys[normalizeName(name)] {
			continue
		}
		p := payload{name: name, file: f.rel, fields: fieldSet{}}
		for j := i + 1; j < len(f.lines) && !closeBodyRe.MatchString(f.lines[j]); j++ {
			if fm := tsFieldRe.FindStringSubmatch(f.lines[j]); fm != nil {
				p.fields[fm[1]] = strings.TrimSpace(fm[2])
			}
		}
		out = append(out, p)
	}
	return out
}

func jsonSchemaPayload(root string, f sourceFile, keys map[string]bool) (payload, bool) {
	b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(f.rel)))
	if err != nil {
		return payload{}, false
	}
	var doc map[string]any
	if json.Unmarshal(b, &doc) != nil {
		return payload{}, false
	}
	props, ok := doc["properties"].(map[string]any)
	if !ok {
		return payload{}, false
	}
	title, _ := doc["title"].(string)
	stem := strings.TrimSuffix(filepath.Base(f.rel), filepath.Ext(f.rel))
	if !keys[normalizeName(title)] && !keys[normalizeName(stem)] {
		return payload{}, false
	}
	p := payload{name: title, file: f.rel, fields: fieldSet{}}
	if p.name == "" {
		p.name = stem
	}
	for name, v := range props {
		p.fields[name] = schemaType(doc, v)
	}
	return p, true
}

// mergePayloads unions the fields of every definition found (a Go producer
// and its TS client type of one event agree, or the diff says so).
func mergePayloads(ps []payload) (fieldSet, string) {
	if len(ps) == 0 {
		return nil, ""
	}
	out := fieldSet{}
	files := map[string]bool{}
	var names []string
	for _, p := range ps {
		for k, v := range p.fields {
			if _, ok := out[k]; !ok {
				out[k] = v
			}
		}
		if !files[p.file] {
			files[p.file] = true
			names = append(names, p.file)
		}
	}
	sort.Strings(names)
	return out, strings.Join(names, ", ")
}

func checkEvent(ctx context.Context, res Result, producer, consumer Side) Result {
	if producer.Base == "" {
		return unchecked(res, "the producer repository has no pinned base to diff its head against")
	}
	aliases := eventAliases(ctx, res.Contract.Name, producer.Base, producer.Head)
	keys := aliasKeys(aliases)
	baseFields, _ := mergePayloads(findPayloads(producer.Base, keys))
	headFields, headFile := mergePayloads(findPayloads(producer.Head, keys))
	switch {
	case baseFields == nil && headFields == nil:
		return unchecked(res, fmt.Sprintf("no payload definition for %s in the producer (a JSON Schema, Go struct or TypeScript type named after the event)", strings.Join(aliases, " / ")))
	case baseFields == nil:
		res.Details = append(res.Details, "the producer adds this payload; nothing the consumer used was removed")
		return finish(res)
	case headFields == nil:
		headFields = fieldSet{}
		headFile = "its definition was removed"
	}
	changes := fieldChanges(baseFields, headFields, res.Contract.Name+" payload", headFile)
	if len(changes) == 0 {
		return finish(res)
	}

	// The consumer's decoding sites: files that name the event, or define a
	// payload type of the same name.
	var refRes []*regexp.Regexp
	for _, a := range aliases {
		refRes = append(refRes, aliasRefPattern(a))
	}
	var sites []sourceFile
	for _, f := range readSources(consumer.Head, consumerExts) {
		hit := false
		for _, l := range f.lines {
			for _, re := range refRes {
				if re.MatchString(l) {
					hit = true
					break
				}
			}
			if hit {
				break
			}
		}
		if !hit {
			for _, l := range f.lines {
				if m := goStructRe.FindStringSubmatch(l); m != nil && keys[normalizeName(m[1])] {
					hit = true
					break
				}
				if m := tsTypeRe.FindStringSubmatch(l); m != nil && (keys[normalizeName(m[1])] || keys[normalizeName(m[2])]) {
					hit = true
					break
				}
			}
		}
		if hit {
			sites = append(sites, f)
		}
	}
	if f, _, ok := codexConsumerFile(consumer.Head, res.Contract); ok && !hasFile(sites, f.rel) {
		sites = append(sites, f)
	}
	if len(sites) == 0 {
		// Not seeing the consumer's decoding site is not evidence that it
		// has none.
		return unchecked(res, fmt.Sprintf("the producer changed %d payload field(s), but no decoding site of the event was found in the consumer head", len(changes)))
	}
	for _, field := range sortedKeys(changes) {
		for _, f := range sites {
			if idx, ok := findFieldUse(f, field); ok && len(res.Mismatches) < maxMismatches {
				res.Mismatches = append(res.Mismatches, Mismatch{ProducerChange: changes[field], ConsumerRef: location(f, idx)})
			}
		}
	}
	if len(res.Mismatches) == 0 {
		res.Details = append(res.Details, fmt.Sprintf("the producer changed %d payload field(s) the consumer does not decode", len(changes)))
	}
	return finish(res)
}

// aliasRefPattern matches a reference to an event alias. An identifier
// alias (TopicUserCreated) may be package-qualified (events.TopicUserCreated);
// a dotted topic value (user.created) must be neither the tail nor the head
// of a longer dotted name (foo.user.created, user.created.v2 are other
// topics); a trailing period that ends a sentence is not part of a name.
func aliasRefPattern(a string) *regexp.Regexp {
	before, after := `(?:^|[^\w.])`, `(?:$|[^\w.]|\.(?:$|[^\w]))`
	if identRe.MatchString(a) {
		before, after = `(?:^|[^\w])`, `(?:$|[^\w])`
	}
	return regexp.MustCompile(before + regexp.QuoteMeta(a) + after)
}

var identRe = regexp.MustCompile(`^[A-Za-z_]\w*$`)
