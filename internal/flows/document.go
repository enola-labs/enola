package flows

import "encoding/json"

// Document is the shape of flows.json: every flow of a snapshot, ranked, the first of
// them with their stages and summary, and the counts of the entries they came from.
type Document struct {
	// Roots counts the entries found, per mechanism.
	Roots map[string]int `json:"roots"`
	// Leaves counts entries that reach nothing the graph follows.
	Leaves int `json:"leaves"`
	// Total is how many flows the entries produced, which is how many Flows holds.
	Total int `json:"total"`
	// Described is how many of the first flows carry stages and a summary.
	Described int    `json:"described"`
	Flows     []Flow `json:"flows"`
}

// Document returns every flow, the first described of them with stages and a
// summary. A flow ranked low is often one a reader looks for by name, sign-in on a
// large application, so it is kept; a stage tree costs a walk per candidate callee, so
// only the first are described.
func (a *Analysis) Document(described int) Document {
	if described > len(a.Flows) {
		described = len(a.Flows)
	}
	a.Describe(described)
	// Never null: a consumer iterating the parsed value must not need a nil
	// check on exactly the repositories that have no flows.
	flows := make([]Flow, len(a.Flows))
	copy(flows, a.Flows)
	return Document{Roots: a.Roots, Leaves: a.Leaves, Total: len(a.Flows), Described: described, Flows: flows}
}

// JSON renders the document as the bytes of flows.json.
func (d Document) JSON() ([]byte, error) {
	out, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(out, '\n'), nil
}
