package sql_schemas

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"

	"github.com/autonomousengine/autonomous-engine/internal/codex/extractors/framework"
)

// Extractor detects SQL schema definitions (CREATE TABLE, ALTER TABLE) in
// migration files (producers) and finds references to those tables in ORM
// models and raw SQL strings (consumers).
type Extractor struct{}

func New() *Extractor { return &Extractor{} }

func init() { framework.Register(New()) }

func (*Extractor) Name() string         { return "contracts.sql_schemas" }
func (*Extractor) Family() string       { return "contracts" }
func (*Extractor) Kind() framework.Kind { return framework.KindStatic }

var caresGlobs = []string{
	"migrations/**/*.sql", "**/*.sql", "**/*.go", "**/*.ts", "**/*.py", "schema.prisma",
}

func (*Extractor) CaresAbout(d *framework.GitDiff) bool {
	if d == nil {
		return true
	}
	return d.HasTouched(caresGlobs)
}

// Producer patterns for SQL DDL.
var (
	createTableRe = regexp.MustCompile(`(?i)CREATE\s+TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?(\w+)`)
	alterTableRe  = regexp.MustCompile(`(?i)ALTER\s+TABLE\s+(\w+)\s+ADD\s+COLUMN\s+(\w+)`)
)

// Consumer patterns for ORM and raw SQL references.
var (
	// Prisma: model Foo { ... }
	prismaModelRe = regexp.MustCompile(`model\s+(\w+)\s*\{`)
	// Drizzle: pgTable('tablename',
	drizzleTableRe = regexp.MustCompile(`pgTable\s*\(\s*['"](\w+)['"]`)
	// GORM: `gorm:"column:colname"`
	gormColumnRe = regexp.MustCompile("`" + `gorm:"[^"]*column:(\w+)[^"]*"` + "`")
	// SQLAlchemy: Table('tablename',
	sqlalchemyTableRe = regexp.MustCompile(`Table\s*\(\s*['"](\w+)['"]`)
)

func (e *Extractor) ProducerExtract(ctx context.Context, ec *framework.ExtractionContext) ([]*framework.ProducerContract, error) {
	var producers []*framework.ProducerContract

	err := filepath.WalkDir(ec.RepoPath, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			base := d.Name()
			if base == "node_modules" || base == ".git" || base == "vendor" {
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(d.Name()) != ".sql" {
			return nil
		}

		rel, _ := filepath.Rel(ec.RepoPath, path)
		rel = filepath.ToSlash(rel)

		f, ferr := os.Open(path)
		if ferr != nil {
			return nil
		}
		defer f.Close()

		scanner := bufio.NewScanner(f)
		lineNum := 0

		for scanner.Scan() {
			lineNum++
			line := scanner.Text()

			if m := createTableRe.FindStringSubmatch(line); m != nil {
				producers = append(producers, &framework.ProducerContract{
					Type: "sql.table",
					Name: m[1],
					File: rel,
					Line: lineNum,
				})
				continue
			}

			if m := alterTableRe.FindStringSubmatch(line); m != nil {
				producers = append(producers, &framework.ProducerContract{
					Type: "sql.column",
					Name: m[1] + "." + m[2],
					File: rel,
					Line: lineNum,
				})
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	return producers, nil
}

func (e *Extractor) ConsumerScan(ctx context.Context, ec *framework.ExtractionContext, producers []*framework.ProducerContract) ([]*framework.Blob, error) {
	// Build set of table names from producers.
	tableNames := map[string]bool{}
	for _, p := range producers {
		if p.Type == "sql.table" {
			tableNames[strings.ToLower(p.Name)] = true
		} else if p.Type == "sql.column" {
			// Extract table name from "table.column"
			parts := strings.SplitN(p.Name, ".", 2)
			if len(parts) > 0 {
				tableNames[strings.ToLower(parts[0])] = true
			}
		}
	}

	type ConsumerEdge struct {
		Producer string `json:"producer"`
		Consumer string `json:"consumer_file"`
		Line     int    `json:"line"`
		Pattern  string `json:"pattern"`
	}

	var edges []ConsumerEdge

	codeExts := map[string]bool{".go": true, ".ts": true, ".py": true, ".prisma": true}

	_ = filepath.WalkDir(ec.RepoPath, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			base := d.Name()
			if base == "node_modules" || base == ".git" || base == "vendor" || base == "migrations" {
				return filepath.SkipDir
			}
			return nil
		}

		ext := filepath.Ext(d.Name())
		if !codeExts[ext] {
			return nil
		}

		rel, _ := filepath.Rel(ec.RepoPath, path)
		rel = filepath.ToSlash(rel)

		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil
		}
		lines := strings.Split(string(data), "\n")

		for i, line := range lines {
			// Check ORM patterns
			for _, re := range []*regexp.Regexp{prismaModelRe, drizzleTableRe, sqlalchemyTableRe} {
				if m := re.FindStringSubmatch(line); m != nil {
					name := strings.ToLower(m[1])
					if tableNames[name] {
						edges = append(edges, ConsumerEdge{
							Producer: m[1],
							Consumer: rel,
							Line:     i + 1,
							Pattern:  "orm-model",
						})
					}
				}
			}

			// Check GORM column tags
			if m := gormColumnRe.FindStringSubmatch(line); m != nil {
				edges = append(edges, ConsumerEdge{
					Producer: m[1],
					Consumer: rel,
					Line:     i + 1,
					Pattern:  "gorm-column",
				})
			}

			// Check raw SQL references to known table names
			for tableName := range tableNames {
				if containsTableReference(line, tableName) {
					edges = append(edges, ConsumerEdge{
						Producer: tableName,
						Consumer: rel,
						Line:     i + 1,
						Pattern:  "raw-sql",
					})
				}
			}
		}
		return nil
	})

	if len(edges) == 0 {
		return nil, nil
	}

	payload, _ := json.Marshal(edges)
	return []*framework.Blob{{
		Section:    "contract_edges",
		Payload:    payload,
		Confidence: 0.80,
	}}, nil
}

// containsTableReference checks if a line contains a SQL-like reference to the
// given table name (e.g., FROM tablename, INTO tablename, JOIN tablename).
func containsTableReference(line, table string) bool {
	lower := strings.ToLower(line)
	keywords := []string{"from ", "join ", "into ", "update ", "table "}
	for _, kw := range keywords {
		idx := strings.Index(lower, kw)
		if idx < 0 {
			continue
		}
		after := strings.TrimSpace(lower[idx+len(kw):])
		if strings.HasPrefix(after, table) {
			rest := after[len(table):]
			if rest == "" || !isIdentChar(rune(rest[0])) {
				return true
			}
		}
	}
	return false
}

func isIdentChar(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_'
}

func (e *Extractor) ExtractFull(ctx context.Context, ec *framework.ExtractionContext) ([]*framework.Blob, error) {
	producers, err := e.ProducerExtract(ctx, ec)
	if err != nil {
		return nil, err
	}
	if len(producers) == 0 {
		return nil, nil
	}
	payload, _ := json.Marshal(producers)
	return []*framework.Blob{{
		Section:    "contract_edges",
		Payload:    payload,
		Confidence: 0.90,
	}}, nil
}

func (e *Extractor) ExtractIncremental(ctx context.Context, ec *framework.ExtractionContext, prev []*framework.Blob) ([]*framework.Blob, error) {
	return e.ExtractFull(ctx, ec)
}
