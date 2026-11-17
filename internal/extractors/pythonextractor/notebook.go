package pythonextractor

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
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
	var doc notebookDoc
	if err := json.Unmarshal(src, &doc); err != nil {
		return nil
	}
	var b bytes.Buffer
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
