package flows

import "encoding/json"

// Document is the shape of flows.json: the largest flows of a snapshot, each
// with its stages and summary, and the counts that say what they were cut from.
type Document struct {
	// Roots counts the entries found, per mechanism.
	Roots map[string]int `json:"roots"`
	// Leaves counts entries that reach nothing the graph follows.
	Leaves int `json:"leaves"`
	// Total is how many flows the entries produced; Flows holds the first of them.
	Total int    `json:"total"`
	Flows []Flow `json:"flows"`
}

// Document describes the first limit flows and returns them as a document.
func (a *Analysis) Document(limit int) Document {
	if limit > len(a.Flows) {
		limit = len(a.Flows)
	}
	a.Describe(limit)
	// Never null: a consumer iterating the parsed value must not need a nil
	// check on exactly the repositories that have no flows.
	flows := make([]Flow, limit)
	copy(flows, a.Flows[:limit])
	return Document{Roots: a.Roots, Leaves: a.Leaves, Total: len(a.Flows), Flows: flows}
}

// JSON renders the document as the bytes of flows.json.
func (d Document) JSON() ([]byte, error) {
	out, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(out, '\n'), nil
}
