// Package sqlextractor models tables declared or changed by SQL migrations.
package sqlextractor

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/enola-labs/enola/internal/factpath"
	"github.com/enola-labs/enola/internal/facts"
)

type Extractor struct{}

func New() *Extractor                                     { return &Extractor{} }
func (e *Extractor) Name() string                         { return "sql" }
func (e *Extractor) Detect(repoPath string) (bool, error) { return false, nil }
func (e *Extractor) DetectFiles(_ string, files []string) (bool, error) {
	for _, file := range files {
		if strings.EqualFold(filepath.Ext(file), ".sql") {
			return true, nil
		}
	}
	return false, nil
}
func (e *Extractor) OwnsFile(file string) bool { return strings.EqualFold(filepath.Ext(file), ".sql") }

// The deliberately small grammar covers migration DDL across PostgreSQL, MySQL,
// SQLite and ClickHouse. Query text is not treated as a declaration: a table being
// selected does not mean the repository owns its schema.
var ddlTable = regexp.MustCompile(`(?is)\b(CREATE|ALTER)\s+TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?((?:[A-Za-z_][\w$]*\.)?[A-Za-z_][\w$]*|"[^"]+"(?:\."[^"]+")?|` + "`[^`]+`(?:\\.`[^`]+`)?" + `)`)

func (e *Extractor) Extract(ctx context.Context, repoPath string, files []string) ([]facts.Fact, error) {
	byName := map[string]facts.Fact{}
	for _, rel := range files {
		if !e.OwnsFile(rel) {
			continue
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		data, err := os.ReadFile(filepath.Join(repoPath, filepath.FromSlash(rel)))
		if err != nil {
			continue
		}
		src := string(data)
		for _, m := range ddlTable.FindAllStringSubmatchIndex(src, -1) {
			op := strings.ToLower(src[m[2]:m[3]])
			table := strings.ReplaceAll(strings.ReplaceAll(src[m[4]:m[5]], "\"", ""), "`", "")
			dir := factpath.Dir(filepath.ToSlash(rel))
			name := dir + "." + table
			candidate := facts.Fact{
				Kind: facts.KindStorage, Name: dir + "." + table, File: filepath.ToSlash(rel),
				Line:      strings.Count(src[:m[0]], "\n") + 1,
				Props:     map[string]any{"storage_kind": "table", "language": "sql", "framework": "sql-ddl", "table": table, "operation": op},
				Relations: []facts.Relation{{Kind: facts.RelDeclares, Target: dir}},
			}
			prior, exists := byName[name]
			// One table is one architectural storage node even when hundreds of
			// migrations alter it. Prefer its CREATE declaration as provenance.
			if !exists || (prior.PropString("operation") != "create" && op == "create") {
				byName[name] = candidate
			}
		}
	}
	out := make([]facts.Fact, 0, len(byName))
	for _, f := range byName {
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].File != out[j].File {
			return out[i].File < out[j].File
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}
