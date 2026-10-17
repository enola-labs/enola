// Package docsjsonl writes docs.jsonl: the comment each documented symbol and module
// carries, in full, one fact per line.
//
// facts.jsonl holds a fact's doc, the first paragraph of its comment, because every
// reader of the graph pays for what it holds. A reader that indexes the comments
// themselves wants all of the text, and reads it here, joined to facts.jsonl by id.
package docsjsonl

import (
	"bytes"
	"context"
	"encoding/json"
	"sort"

	"github.com/enola-labs/enola/internal/facts"
)

// Name is the renderer's name in a config's renderers list.
const Name = "docs"

// ArtifactName is the file the renderer writes.
const ArtifactName = "docs.jsonl"

// Line is one line of docs.jsonl. ID is the fact's id in facts.jsonl; Doc is the
// whole comment, or the fact's doc when that is all of it.
type Line struct {
	ID   string `json:"id"`
	Repo string `json:"repo,omitempty"`
	Kind string `json:"kind"`
	Name string `json:"name"`
	File string `json:"file"`
	Line int    `json:"line,omitempty"`
	Doc  string `json:"doc"`
}

type Renderer struct{}

func New() *Renderer { return &Renderer{} }

func (r *Renderer) Name() string { return Name }

// Render writes nothing: the full texts are in the store's side table, not on the
// snapshot's facts. See RenderStore.
func (r *Renderer) Render(context.Context, *facts.Snapshot) ([]facts.Artifact, error) {
	return nil, nil
}

// RenderStore produces docs.jsonl, sorted by id, then line, then text, with exact
// repeats dropped: two overloads share an id, and the same comment recorded twice
// is one line. It is written empty for a snapshot with no documented fact, so a
// consumer can tell "none" from "not run".
func (r *Renderer) RenderStore(_ context.Context, _ *facts.Snapshot, store *facts.Store) ([]facts.Artifact, error) {
	var lines []Line
	for _, f := range store.FactsRef() {
		doc := f.PropString(facts.PropDoc)
		if doc == "" {
			continue
		}
		if full := store.DocText(facts.FactKey{Repo: f.Repo, Kind: f.Kind, Name: f.Name, File: f.File}); full != "" {
			doc = full
		}
		lines = append(lines, Line{
			ID: facts.FactID(f.Repo, f.Kind, f.Name, f.File), Repo: f.Repo, Kind: f.Kind,
			Name: f.Name, File: f.File, Line: f.Line, Doc: doc,
		})
	}
	sort.Slice(lines, func(i, j int) bool {
		a, b := lines[i], lines[j]
		if a.ID != b.ID {
			return a.ID < b.ID
		}
		if a.Line != b.Line {
			return a.Line < b.Line
		}
		return a.Doc < b.Doc
	})
	var buf bytes.Buffer
	for i, l := range lines {
		if i > 0 && l == lines[i-1] {
			continue
		}
		b, err := json.Marshal(l)
		if err != nil {
			return nil, err
		}
		buf.Write(b)
		buf.WriteByte('\n')
	}
	return []facts.Artifact{{Name: ArtifactName, Content: buf.Bytes(), Type: "application/x-ndjson"}}, nil
}
