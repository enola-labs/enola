// Package flowsjson writes flows.json: the ways into the program a snapshot
// describes, ranked by what each reaches.
package flowsjson

import (
	"context"

	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/internal/flows"
)

// Name is the renderer's name in a config's renderers list.
const Name = "flows"

// ArtifactName is the file the renderer writes.
const ArtifactName = "flows.json"

// Limit is how many flows the artifact holds. A repository can have a flow per
// route, and a reader of the file wants the ones that matter, with their
// stages; the document's total says how many there were.
const Limit = 50

type Renderer struct{}

func New() *Renderer { return &Renderer{} }

func (r *Renderer) Name() string { return Name }

// Render writes nothing: the ranking walks calls by name, which needs the
// store's index and not the snapshot's flat fact list. See RenderStore.
func (r *Renderer) Render(context.Context, *facts.Snapshot) ([]facts.Artifact, error) {
	return nil, nil
}

// RenderStore produces flows.json. It is written for a snapshot with no flows
// too, with an empty list, so a consumer can tell "none found" from "not run".
func (r *Renderer) RenderStore(_ context.Context, _ *facts.Snapshot, store *facts.Store) ([]facts.Artifact, error) {
	doc, err := flows.Analyze(store, flows.Options{}).Document(Limit).JSON()
	if err != nil {
		return nil, err
	}
	return []facts.Artifact{{Name: ArtifactName, Content: doc, Type: "application/json"}}, nil
}
