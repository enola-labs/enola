package history

import (
	"encoding/json"
	"fmt"
)

// ShortBlameLine renders one canonical fact or finding line by the fields that identify
// it: kind, name and file:line, or a finding's title. A fact line carries its full props
// and relations, and an answer to "when" should not bury itself under that JSON.
func ShortBlameLine(line string) string {
	var f struct {
		Kind  string `json:"kind"`
		Name  string `json:"name"`
		File  string `json:"file"`
		Line  int    `json:"line"`
		Title string `json:"title"`
	}
	if err := json.Unmarshal([]byte(line), &f); err != nil {
		return truncate(line, 120)
	}
	if f.Title != "" { // a finding
		return f.Title
	}
	where := f.File
	if where != "" && f.Line > 0 {
		where = fmt.Sprintf("%s:%d", f.File, f.Line)
	}
	if where == "" {
		return fmt.Sprintf("%-11s %s", f.Kind, f.Name)
	}
	return fmt.Sprintf("%-11s %s  (%s)", f.Kind, f.Name, where)
}

// ShortBlamePair renders a changed symbol as its new line and where it was before, which
// is the part of the old line a reader needs once the pair says it is the same symbol.
func ShortBlamePair(p BlamePair) string {
	var from struct {
		File string `json:"file"`
		Line int    `json:"line"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal([]byte(p.From), &from); err != nil {
		return ShortBlameLine(p.To)
	}
	was := fmt.Sprintf("%s:%d", from.File, from.Line)
	if p.Kind == PairMoved {
		was = from.Name + "  (" + was + ")"
	}
	return ShortBlameLine(p.To) + "  was " + was
}
