package contracts

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/autonomousengine/autonomous-engine/internal/codex/extractors/contracts/sql_schemas"
	"github.com/autonomousengine/autonomous-engine/internal/codex/extractors/framework"
)

// The db_schema checker. A schema contract is named by its table ("users",
// "public.users") or one column of it ("users.email", the codex sql_schemas
// extractor's column naming). The producer's change is the SQL its head adds
// over its pinned base: every statement in a .sql file of the head that is
// not in the base (down migrations excluded — they undo, they do not ship).
// The checker looks in those statements for
//
//   - DROP TABLE t / ALTER TABLE t RENAME TO u: every consumer line that
//     queries t is a mismatch;
//   - ALTER TABLE t DROP COLUMN c / RENAME [COLUMN] c TO d: a consumer line
//     that names c within a few lines of a query on t is a mismatch (first
//     use per file).
//
// A producer with no SQL at all is unchecked. Type changes (ALTER COLUMN
// ... TYPE), views, and schema changes made outside .sql files (ORM
// migrations) are not detected; see the spec.

var (
	stmtSplitRe    = regexp.MustCompile(`;`)
	sqlCommentRe   = regexp.MustCompile(`(?m)--.*$`)
	dropTableRe    = regexp.MustCompile(`(?i)^drop\s+table\s+(?:if\s+exists\s+)?(.+?)(?:\s+(?:cascade|restrict))?$`)
	alterTableRe   = regexp.MustCompile(`(?i)^alter\s+table\s+(?:if\s+exists\s+)?(?:only\s+)?([\w."]+)\s+(.*)$`)
	dropColumnRe   = regexp.MustCompile(`(?i)drop\s+(?:column\s+)?(?:if\s+exists\s+)?([\w"]+)`)
	renameColumnRe = regexp.MustCompile(`(?i)rename\s+(?:column\s+)?([\w"]+)\s+to\s+([\w"]+)`)
	renameTableRe  = regexp.MustCompile(`(?i)^rename\s+to\s+([\w."]+)`)
)

// tableWindow is how many lines around a query on the table a column use
// still counts as part of that query.
const tableWindow = 12

func unquoteIdent(s string) string {
	s = strings.Trim(strings.TrimSpace(s), `"`+"`")
	if i := strings.LastIndex(s, "."); i >= 0 {
		s = s[i+1:]
	}
	return strings.ToLower(strings.Trim(s, `"`+"`"))
}

// parseSchemaName splits a contract name into table and optional column.
func parseSchemaName(name string) (table, column string) {
	parts := strings.Split(strings.ToLower(strings.TrimSpace(name)), ".")
	switch {
	case len(parts) == 1:
		return unquoteIdent(parts[0]), ""
	case len(parts) == 2 && parts[0] == "public":
		return unquoteIdent(parts[1]), ""
	case len(parts) == 2:
		return unquoteIdent(parts[0]), unquoteIdent(parts[1])
	default: // schema.table.column
		return unquoteIdent(parts[len(parts)-2]), unquoteIdent(parts[len(parts)-1])
	}
}

func isDownMigration(rel string) bool {
	l := strings.ToLower(rel)
	return strings.HasSuffix(l, ".down.sql") || strings.HasSuffix(l, "_down.sql") || strings.Contains(l, "/down/")
}

// sqlStatements is every statement of the tree's up-direction .sql files,
// normalised (comments stripped, whitespace collapsed), with the file it
// came from.
func sqlStatements(root string) (map[string]string, int) {
	out := map[string]string{}
	files := 0
	for _, f := range readSources(root, map[string]bool{".sql": true}) {
		if isDownMigration(f.rel) {
			continue
		}
		files++
		body := sqlCommentRe.ReplaceAllString(strings.Join(f.lines, "\n"), "")
		for _, s := range stmtSplitRe.Split(body, -1) {
			n := strings.Join(strings.Fields(s), " ")
			if n == "" {
				continue
			}
			if _, ok := out[n]; !ok {
				out[n] = f.rel
			}
		}
	}
	return out, files
}

// schemaChange is one destructive change to the contract's table.
type schemaChange struct {
	column string // "" for a table-wide change
	what   string
}

func checkDBSchema(ctx context.Context, res Result, producer, consumer Side) Result {
	table, column := parseSchemaName(res.Contract.Name)
	if table == "" {
		return unchecked(res, fmt.Sprintf("the schema contract name %q names no table", res.Contract.Name))
	}
	if producer.Base == "" {
		return unchecked(res, "the producer repository has no pinned base to diff its head against")
	}
	headStmts, headFiles := sqlStatements(producer.Head)
	baseStmts, _ := sqlStatements(producer.Base)
	if headFiles == 0 {
		return unchecked(res, "the producer head has no SQL migration or schema files the checker parses")
	}

	var changes []schemaChange
	for stmt, file := range headStmts {
		if _, old := baseStmts[stmt]; old {
			continue
		}
		if m := dropTableRe.FindStringSubmatch(stmt); m != nil {
			for _, t := range strings.Split(m[1], ",") {
				if unquoteIdent(t) == table {
					changes = append(changes, schemaChange{what: fmt.Sprintf("dropped table %s (%s)", table, file)})
				}
			}
			continue
		}
		m := alterTableRe.FindStringSubmatch(stmt)
		if m == nil || unquoteIdent(m[1]) != table {
			continue
		}
		actions := m[2]
		if rt := renameTableRe.FindStringSubmatch(actions); rt != nil {
			changes = append(changes, schemaChange{what: fmt.Sprintf("renamed table %s to %s (%s)", table, unquoteIdent(rt[1]), file)})
			continue
		}
		for _, rc := range renameColumnRe.FindAllStringSubmatch(actions, -1) {
			col := unquoteIdent(rc[1])
			changes = append(changes, schemaChange{column: col, what: fmt.Sprintf("renamed column %s.%s to %s (%s)", table, col, unquoteIdent(rc[2]), file)})
		}
		for _, part := range strings.Split(actions, ",") {
			part = strings.TrimSpace(part)
			if !strings.HasPrefix(strings.ToLower(part), "drop") || strings.HasPrefix(strings.ToLower(part), "drop constraint") ||
				strings.HasPrefix(strings.ToLower(part), "drop default") || strings.HasPrefix(strings.ToLower(part), "drop not null") {
				continue
			}
			if dc := dropColumnRe.FindStringSubmatch(part); dc != nil {
				col := unquoteIdent(dc[1])
				changes = append(changes, schemaChange{column: col, what: fmt.Sprintf("dropped column %s.%s (%s)", table, col, file)})
			}
		}
	}
	if column != "" {
		kept := changes[:0]
		for _, c := range changes {
			if c.column == "" || c.column == column {
				kept = append(kept, c)
			}
		}
		changes = kept
	}
	if len(changes) == 0 {
		if !tableKnown(ctx, table, producer) {
			// The table is defined somewhere the checker does not read (an
			// ORM, a declarative schema): no evidence either way.
			return unchecked(res, fmt.Sprintf("the producer's SQL defines no table %s (it may be defined outside .sql files), so changes to it cannot be seen", table))
		}
		return finish(res)
	}

	tableRe := regexp.MustCompile(`(?i)\b(?:from|join|into|update|table)\s+["\x60]?(?:\w+["\x60]?\.["\x60]?)?` + regexp.QuoteMeta(table) + `\b`)
	for _, f := range readSources(consumer.Head, consumerExts) {
		var tableLines []int
		for i, l := range f.lines {
			if tableRe.MatchString(l) {
				tableLines = append(tableLines, i)
			}
		}
		if len(tableLines) == 0 {
			continue
		}
		for _, c := range changes {
			if len(res.Mismatches) >= maxMismatches {
				break
			}
			if c.column == "" {
				res.Mismatches = append(res.Mismatches, Mismatch{ProducerChange: c.what, ConsumerRef: location(f, tableLines[0])})
				continue
			}
			if idx, ok := columnUseNear(f, c.column, tableLines); ok {
				res.Mismatches = append(res.Mismatches, Mismatch{ProducerChange: c.what, ConsumerRef: location(f, idx)})
			}
		}
	}
	if len(res.Mismatches) == 0 {
		res.Details = append(res.Details, fmt.Sprintf("the producer changes table %s, but the consumer head does not query what changed", table))
	}
	return finish(res)
}

// columnUseNear returns the first line naming column within tableWindow
// lines of a query on the table.
func columnUseNear(f sourceFile, column string, tableLines []int) (int, bool) {
	re := regexp.MustCompile(`(?i)(?:^|[^\w])` + regexp.QuoteMeta(column) + `(?:$|[^\w])`)
	for i, l := range f.lines {
		if !re.MatchString(l) {
			continue
		}
		for _, t := range tableLines {
			if i >= t-tableWindow && i <= t+tableWindow {
				return i, true
			}
		}
	}
	return 0, false
}

// tableKnown reports whether the sql_schemas extractor sees the table
// created in the producer's base or head.
func tableKnown(ctx context.Context, table string, producer Side) bool {
	for _, root := range []string{producer.Base, producer.Head} {
		if root == "" {
			continue
		}
		ps, err := sql_schemas.New().ProducerExtract(ctx, &framework.ExtractionContext{RepoPath: root})
		if err != nil {
			continue
		}
		for _, p := range ps {
			if p.Type == "sql.table" && strings.EqualFold(p.Name, table) {
				return true
			}
		}
	}
	return false
}
