package pythonextractor

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/internal/parallel"
)

// Jupyter notebooks hold Python in their code cells. A notebook repository keeps
// its helpers in modules beside the notebooks and calls them from the cells, so
// with the notebooks unread every helper looks unused. A notebook is read as one
// reference-only fact: the production symbols its cells call, resolved like the
// references from a test, and nothing it declares itself.

// isNotebook reports a Jupyter notebook, outside the checkpoint copies Jupyter
// saves beside it.
func isNotebook(path string) bool {
	return strings.HasSuffix(strings.ToLower(path), ".ipynb") && !strings.Contains(path, ".ipynb_checkpoints/")
}

type notebookDoc struct {
	Cells []struct {
		CellType string          `json:"cell_type"`
		Source   json.RawMessage `json:"source"`
	} `json:"cells"`
}

// notebookSource joins a notebook's code cells into one Python source. IPython
// magics and shell escapes (`%matplotlib inline`, `!pip install x`) are not
// Python and are left out.
func notebookSource(src []byte) []byte {
	var b bytes.Buffer
	for _, text := range codeCells(src) {
		for _, line := range strings.Split(text, "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "%") || strings.HasPrefix(trimmed, "!") {
				continue
			}
			b.WriteString(line)
			b.WriteByte('\n')
		}
		b.WriteByte('\n')
	}
	return b.Bytes()
}

// codeCells returns the text of a notebook's code cells, magics included.
func codeCells(src []byte) []string {
	var doc notebookDoc
	if err := json.Unmarshal(src, &doc); err != nil {
		return nil
	}
	var out []string
	for _, c := range doc.Cells {
		if c.CellType != "code" {
			continue
		}
		var text string
		var lines []string
		if err := json.Unmarshal(c.Source, &lines); err == nil {
			text = strings.Join(lines, "")
		} else if err := json.Unmarshal(c.Source, &text); err != nil {
			continue
		}
		out = append(out, text)
	}
	return out
}

// A notebook mostly reads a warehouse: SQL in a string handed to a client, or a
// %%sql / %%bigquery cell. A table is named after FROM or JOIN, qualified
// (`project.dataset.table`, dataset.table), possibly with a placeholder for the
// project (`{project}.marts.orders`). A cell with no SELECT holds no query, and a
// Python `from a.b import c` is not one.
var (
	sqlTableRef   = regexp.MustCompile("(?i)\\b(?:from|join)\\s+(`[^`\\n]+`|[A-Za-z0-9_{}$-]+(?:\\.[A-Za-z0-9_{}$-]+)+)")
	sqlSelect     = regexp.MustCompile(`(?i)\bselect\b`)
	pyImportAfter = regexp.MustCompile(`^\s+import\b`)
	sqlTableName  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// notebookTables returns the tables a notebook's queries read, by the table's
// own name (the last segment), as a warehouse model or a BI view names it.
func notebookTables(src []byte) []string {
	seen := map[string]bool{}
	for _, cell := range codeCells(src) {
		if !sqlSelect.MatchString(cell) {
			continue
		}
		for _, m := range sqlTableRef.FindAllStringSubmatchIndex(cell, -1) {
			if pyImportAfter.MatchString(cell[m[1]:]) {
				continue
			}
			ref := strings.Trim(cell[m[2]:m[3]], "`")
			segs := strings.Split(ref, ".")
			if len(segs) < 2 {
				continue
			}
			if table := segs[len(segs)-1]; sqlTableName.MatchString(table) {
				seen[table] = true
			}
		}
	}
	out := make([]string, 0, len(seen))
	for t := range seen {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// notebookTableReads is one fact per notebook whose queries read tables: the
// notebook depends on each, so a walk from a table reaches the notebooks reading
// it.
func notebookTableReads(ctx context.Context, repoPath string, notebooks []string) []facts.Fact {
	perFile := parallel.MapFiles(ctx, notebooks, func(relFile string) []facts.Fact {
		src, err := os.ReadFile(filepath.Join(repoPath, relFile))
		if err != nil {
			return nil
		}
		tables := notebookTables(src)
		if len(tables) == 0 {
			return nil
		}
		f := facts.Fact{
			Kind: facts.KindFileRef, Name: relFile, File: relFile, Line: 1,
			Props: map[string]any{"notebook": true, "language": "python", "reads_tables": strings.Join(tables, " ")},
		}
		for _, t := range tables {
			f.Relations = append(f.Relations, facts.Relation{Kind: facts.RelDependsOn, Target: t})
		}
		return []facts.Fact{f}
	})
	var out []facts.Fact
	for _, ff := range perFile {
		out = append(out, ff...)
	}
	return out
}

// refsFromNotebook reads a notebook's code as a module named after it and keeps
// the references it makes, as one file reference for the notebook.
func refsFromNotebook(relFile string, src []byte) []facts.Fact {
	code := notebookSource(src)
	if len(code) == 0 {
		return nil
	}
	asModule := strings.TrimSuffix(relFile, ".ipynb") + ".py"
	out := refsFromPyTest(code, asModule)
	for i := range out {
		out[i].Kind = facts.KindFileRef
		out[i].Name, out[i].File = relFile, relFile
		out[i].Props["notebook"] = true
	}
	return out
}
