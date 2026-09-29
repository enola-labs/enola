package facts

import "encoding/json"

// The wire shape of facts.jsonl.
//
// It differs from Fact and Relation by exactly the two id fields, and it exists
// as a separate type so those never reach the in-memory model. Embedding rather
// than restating the fields is what makes that safe: a field added to Fact
// appears here automatically, in the same position, so the two cannot drift.
//
// FIELD ORDER MATTERS. encoding/json emits fields in declaration order, so ID
// last keeps every line's prefix byte-identical to what it was before ids
// existed. WriteJSONL sorts the marshalled LINES, so a leading id would have
// re-ordered the whole file — and pkg/history stores each revision as a patch
// over those sorted lines, so the reordering would have cost a full rewrite of
// every stored revision rather than the one fresh base this actually costs.

// wireRelation is a Relation plus the identity of the fact its target names,
// when that name resolves to exactly one.
type wireRelation struct {
	Relation
	// TargetID is omitted rather than emptied when the target does not resolve:
	// most unresolved targets are stdlib or third-party names with no fact in
	// the snapshot at all (42.8% of relations in a measured corpus), and an
	// empty string would present "nothing to point at" and "I could not decide"
	// as the same answer. Absent means "resolve this by name yourself", which is
	// what every consumer already does today.
	TargetID string `json:"target_id,omitempty"`
}

// wireFact is a Fact plus its identity, with its relations widened to carry
// theirs.
type wireFact struct {
	Fact
	// Relations shadows Fact.Relations: a field at depth 0 wins over a promoted
	// one at depth 1, so this is what marshals, under the same key.
	Relations []wireRelation `json:"relations,omitempty"`
	ID        string         `json:"id"`
}

// PropMatchedRoutes is the prop on a client route listing the server routes the
// cross-repo HTTP linker resolved it to. Each entry is a map carrying the target's
// repo, name and file (plus method and confidence), which is its full identity.
//
// It is a prop and not a relation because a relation names its target by NAME, and a
// client call site and the server route it reaches usually share one: from the
// client's repository, name resolution prefers the client fact itself, and the graph,
// which is name-keyed, would draw the link as a self-loop. The entry names the target
// by identity instead, and the writer adds that identity's id beside it.
const PropMatchedRoutes = "matched_routes"

// PropCaller is the prop on a client route naming the symbol whose body makes the
// call: the innermost function or method containing the call site that the
// extractor emitted as a symbol. That symbol is declared in the call site's own
// file, so the writer derives its id from the route's repo and file and writes it
// beside the name as PropCallerID. A prop for the reason PropMatchedRoutes is one:
// a relation to the route would name it by a name other call sites share.
const (
	PropCaller   = "caller"
	PropCallerID = "caller_id"
)

// withFactRefIDs returns a route's props with the ids of the facts they name: an
// "id" on every entry of PropMatchedRoutes, computed from the entry's repo, name and
// file, and PropCallerID for PropCaller, computed from the route's own repo and file.
// Each is derived exactly as the named fact's own id is. It returns props unchanged
// when there is nothing to add, and otherwise a copy: the store's maps are shared and
// must not be written here.
//
// An id already present is recomputed rather than trusted, so a snapshot read back
// from disk and written again cannot carry an id its entry no longer agrees with.
func withFactRefIDs(props map[string]any, repo, file string, scratch []byte) (map[string]any, []byte) {
	refs, hasRefs := props[PropMatchedRoutes].([]any)
	hasRefs = hasRefs && len(refs) > 0
	caller, _ := props[PropCaller].(string)
	_, staleCallerID := props[PropCallerID]
	if !hasRefs && caller == "" && !staleCallerID {
		return props, scratch
	}
	out := make(map[string]any, len(props)+1)
	for k, v := range props {
		out[k] = v
	}
	delete(out, PropCallerID)
	if caller != "" {
		out[PropCallerID], scratch = factIDInto(scratch, repo, KindSymbol, caller, file)
	}
	if !hasRefs {
		return out, scratch
	}
	widened := make([]any, len(refs))
	for i, r := range refs {
		entry, ok := r.(map[string]any)
		if !ok {
			widened[i] = r
			continue
		}
		repo, _ := entry["repo"].(string)
		name, _ := entry["name"].(string)
		file, _ := entry["file"].(string)
		copied := make(map[string]any, len(entry)+1)
		for k, v := range entry {
			copied[k] = v
		}
		if repo != "" && name != "" {
			copied["id"], scratch = factIDInto(scratch, repo, KindRoute, name, file)
		} else {
			delete(copied, "id")
		}
		widened[i] = copied
	}
	out[PropMatchedRoutes] = widened
	return out, scratch
}

// dropWireIDs removes from a fact read back from facts.jsonl the ids only the writer
// adds (PropCallerID, and the id of each PropMatchedRoutes entry), the same way
// target_id is dropped by decoding into Relation. They are derived on every write and
// never stored, so a fact read back must equal the fact that was written: kept, they
// made every client route with a caller or a matched route differ from its in-memory
// self, and a diff against a snapshot on disk reported those routes as changed on a
// tree nothing had touched.
func dropWireIDs(f *Fact) {
	if f.Kind != KindRoute || f.Props == nil {
		return
	}
	delete(f.Props, PropCallerID)
	refs, _ := f.Props[PropMatchedRoutes].([]any)
	for _, r := range refs {
		if entry, ok := r.(map[string]any); ok {
			delete(entry, "id")
		}
	}
}

// targetFactFor resolves a relation target NAME to the index of the fact it
// names, or -1 when the snapshot cannot answer that unambiguously. The caller
// turns the index into an id, so resolution and hashing stay separable and the
// hash can reuse one scratch buffer for the whole pass.
//
// Two ways it declines. The name may match no fact — the common case, and not a
// defect: an edge to fmt.Sprintf or to a third-party package names something
// this repository does not contain. Or it may match facts of more than one
// identity, and there is no honest way to choose; enola's own graph does not
// choose either (it is name-keyed, and reports the residual as Conflated), so
// emitting a pick here would invent a precision the analysis does not have.
//
// Facts in the caller's own repository win over facts elsewhere in a multi-repo
// snapshot, matching how a consumer already resolves these by hand. The result
// does not depend on the order of the byName bucket: an id is returned only when
// every candidate agrees on it.
//
// The caller must hold s.mu.
func (s *Store) targetFactFor(target, fromRepo string) int {
	first, rest, ok := s.namedIdxs(target)
	if !ok {
		return -1
	}

	pick := -1
	if s.facts[first].Repo == fromRepo {
		pick = first
	}
	for _, i32 := range rest {
		i := int(i32)
		if s.facts[i].Repo != fromRepo {
			continue
		}
		if pick == -1 {
			pick = i
			continue
		}
		if !sameIdentity(s.facts[pick], s.facts[i]) {
			return -1 // ambiguous inside the repository that made the reference
		}
	}

	if pick == -1 {
		// No fact in this repository carries the name, so the reference points
		// outward: consider the whole snapshot, under the same all-or-nothing rule.
		pick = first
		for _, i := range rest {
			if !sameIdentity(s.facts[pick], s.facts[i]) {
				return -1
			}
		}
	}

	return pick
}

// The wire shape of insights.json.
//
// Evidence cites a fact by NAME, so a consumer linking a finding to the code it
// is about re-runs the same name matching relations needed, and drops what it
// cannot resolve. fact_id answers it directly, from the same identity the facts
// carry.

// wireEvidence is an Evidence plus the identity of the fact it cites, when that
// citation resolves to exactly one.
//
// Absent is not a defect and must not be read as one. Some findings cite names
// that are SUPPOSED to be missing — "this route names a handler that is not
// defined here" is a finding about absence, and its evidence resolving to
// nothing is the finding being true. Others cite third-party symbols the
// repository calls but does not contain.
type wireEvidence struct {
	Evidence
	FactID string `json:"fact_id,omitempty"`
}

// wireInsight restates Insight's fields instead of embedding it, which is the
// one place this file does not mirror its model type.
//
// encoding/json orders an embedded struct's promoted fields before the outer
// struct's own, so shadowing `evidence` the way wireFact shadows `relations`
// would move it from the middle of the object to the end. That is invisible to a
// parser and expensive to a reader: it rewrites every line of every insights
// golden, which is exactly the diff a change to a finding needs to be legible
// in. TestWireFormat_WireInsightFields pins the two shapes against each other so
// restating them cannot drift.
type wireInsight struct {
	Title         string         `json:"title"`
	Source        string         `json:"source,omitempty"`
	Description   string         `json:"description"`
	Confidence    float64        `json:"confidence"`
	Evidence      []wireEvidence `json:"evidence"`
	Actions       []string       `json:"suggested_actions,omitempty"`
	Informational bool           `json:"informational,omitempty"`
	Metrics       map[string]any `json:"metrics,omitempty"`
}

// MarshalInsights renders insights as the bytes of insights.json, resolving each
// evidence entry's citation to the id of the fact it names.
//
// It marshals the slice it is given rather than normalising it: a nil slice
// still renders as `null`, so the callers keep whatever they promised before
// this existed, and adding ids cannot change anything else about the document.
func (s *Store) MarshalInsights(insights []Insight) ([]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var out []wireInsight
	if insights != nil {
		out = make([]wireInsight, 0, len(insights))
	}
	var scratch []byte
	for _, in := range insights {
		var ev []wireEvidence
		if in.Evidence != nil {
			ev = make([]wireEvidence, 0, len(in.Evidence))
			for _, e := range in.Evidence {
				we := wireEvidence{Evidence: e}
				if i := s.evidenceFactFor(e); i >= 0 {
					f := s.facts[i]
					we.FactID, scratch = factIDInto(scratch, f.Repo, f.Kind, f.Name, f.File)
				}
				ev = append(ev, we)
			}
		}
		out = append(out, wireInsight{
			Title:         in.Title,
			Source:        in.Source,
			Description:   in.Description,
			Confidence:    in.Confidence,
			Evidence:      ev,
			Actions:       in.Actions,
			Informational: in.Informational,
			Metrics:       in.Metrics,
		})
	}
	return json.MarshalIndent(out, "", "  ")
}

// evidenceFactFor resolves the fact an evidence entry cites to its index, or -1
// when the citation does not resolve to exactly one identity.
//
// An entry cites at most one of Symbol or Fact; both are fact names, and the
// kinds they conventionally name differ, not the lookup. Where the entry also
// names a file, that narrows the candidates — but only when some candidate
// agrees, since a finding may render a path differently from the fact it cites.
// In a measured corpus the file was set on 60 of 772 resolvable entries and
// agreed every time, so it is a tiebreaker rather than the mechanism.
//
// The caller must hold s.mu.
func (s *Store) evidenceFactFor(e Evidence) int {
	ref := e.Symbol
	if ref == "" {
		ref = e.Fact
	}
	if ref == "" {
		return -1
	}
	first, rest, ok := s.namedIdxs(ref)
	if !ok {
		return -1
	}

	pick := -1
	if e.File != "" {
		if s.facts[first].File == e.File {
			pick = first
		}
		for _, i32 := range rest {
			i := int(i32)
			if s.facts[i].File != e.File {
				continue
			}
			if pick == -1 {
				pick = i
				continue
			}
			if !sameIdentity(s.facts[pick], s.facts[i]) {
				return -1
			}
		}
	}

	if pick == -1 {
		pick = first
		for _, i := range rest {
			if !sameIdentity(s.facts[pick], s.facts[i]) {
				return -1
			}
		}
	}

	return pick
}
