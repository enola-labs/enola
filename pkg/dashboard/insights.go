package dashboard

import (
	"math"
	"sort"

	"github.com/enola-labs/enola/pkg/facts"
)

// insightGroupPreviewCap bounds how many of a group's items the overview shows
// inline, mirroring pkg/explain's topPerGroup: each explainer already sorts its
// own findings worst-first, so showing the first few and linking to the full
// list (the Insights modal, which still renders every item) turns "91
// candidates" from a wall of rows into something worth reading.
const insightGroupPreviewCap = 5

// insightRow is one insight in a category group of the Insights modal.
type insightRow struct {
	Title         string
	Confidence    int  // 0-100
	Structural    bool // confidence >= 1.0 and not Informational — a proven, non-heuristic finding
	Informational bool // describes the architecture rather than flagging a problem; never gradeable (see pkg/check)
	Evidence      string
	Action        string // the finding's first suggested action, if any — the concrete "what to do about it"
}

// insightGroup is all insights produced by one explainer (Source), with a bar
// width proportional to its share of the largest group.
type insightGroup struct {
	Source        string
	Label         string
	Count         int
	BarPct        int  // 0-100, relative to the largest group
	HasStructural bool // true if any item is a proven (Structural) finding — ranks the group ahead of larger but purely heuristic ones
	Items         []insightRow // full list, for the modal
	Shown         []insightRow // capped to insightGroupPreviewCap, for the overview
	Hidden        int          // len(Items) - len(Shown)
}

// insightLabels maps an explainer Source id to a human-friendly label, one entry
// per explainer bootstrap.NewEngine registers. The engine has no display-name
// registry, so this mirror lives here.
//
// It doubles as the ADMISSION LIST: insightDetails renders only sources found
// here. Both binaries share a repo's .enola/insights.json, so a file written by
// a build with extra explainers must not leak findings this engine cannot
// produce into this engine's dashboard. A wrapper widens the list — labelling
// and admitting in one step — through Options.InsightLabels.
var insightLabels = map[string]string{
	"hotspots":            "Hotspots",
	"god-class":           "God classes",
	"exported-surface":    "Exported surface",
	"complexity-outliers": "Complexity outliers",
	"dependency-depth":    "Dependency depth",
	"cycles":              "Dependency cycles",
	"layers":              "Layer violations",
	"crossrepo":           "Cross-repo dependencies",
	"coverage":            "Coverage gaps",
	"unused-routes":       "Unused routes",
	"domain":              "Domain boundaries",
	"query-loops":         "Query loops",
	"entry-points":        "Entry points",
	"messaging-coverage":  "Messaging coverage",
	"intent":              "Intent",
	"constraints":         "Constraint violations",
}

// mergedLabels returns the engine's label map widened by a wrapper's extra
// entries. The result is a copy, so a Server never mutates package state and two
// dashboards in one process cannot see each other's labels.
func mergedLabels(extra map[string]string) map[string]string {
	merged := make(map[string]string, len(insightLabels)+len(extra))
	for source, label := range insightLabels {
		merged[source] = label
	}
	for source, label := range extra {
		merged[source] = label
	}
	return merged
}

// firstEvidence returns the most locating evidence string for an insight: the first
// evidence's File, else Symbol, else Fact. Empty when there is no evidence.
func firstEvidence(ev []facts.Evidence) string {
	for _, e := range ev {
		switch {
		case e.File != "":
			return e.File
		case e.Symbol != "":
			return e.Symbol
		case e.Fact != "":
			return e.Fact
		}
	}
	return ""
}

// insightDetails groups insights by explainer Source for the modal: one bar per
// group (count-ranked, width relative to the largest group), each expandable to its
// insights (sorted certain-first). It also returns the structural vs candidate split
// (confidence == 1.0 vs < 1.0) shown in the modal header. A nil/empty list yields no
// groups, so the counters stay plain, non-clickable numbers.
//
// labels is both the display-name map and the admission list: an insight whose
// Source is absent from it is dropped, and excluded from the returned counts.
func insightDetails(ins []facts.Insight, labels map[string]string) (groups []insightGroup, structural, candidate int) {
	if len(ins) == 0 {
		return nil, 0, 0
	}

	bySource := make(map[string][]insightRow)
	for _, in := range ins {
		if _, known := labels[in.Source]; !known {
			continue // produced by explainers this build does not have
		}
		conf := int(math.Round(in.Confidence * 100))
		isStructural := in.Confidence >= 1.0 && !in.Informational
		switch {
		case in.Informational:
			// Worth showing, but neither a proven problem nor a candidate to fix —
			// pkg/check never grades these either. Counted in neither bucket.
		case isStructural:
			structural++
		default:
			candidate++
		}
		var action string
		if len(in.Actions) > 0 {
			action = in.Actions[0]
		}
		bySource[in.Source] = append(bySource[in.Source], insightRow{
			Title:         in.Title,
			Confidence:    conf,
			Structural:    isStructural,
			Informational: in.Informational,
			Evidence:      firstEvidence(in.Evidence),
			Action:        action,
		})
	}

	maxCount := 1
	for _, items := range bySource {
		if len(items) > maxCount {
			maxCount = len(items)
		}
	}

	for source, items := range bySource {
		sort.Slice(items, func(i, j int) bool {
			if items[i].Confidence != items[j].Confidence {
				return items[i].Confidence > items[j].Confidence
			}
			return items[i].Title < items[j].Title
		})
		hasStructural := false
		for _, it := range items {
			if it.Structural {
				hasStructural = true
				break
			}
		}
		shown, hidden := items, 0
		if len(items) > insightGroupPreviewCap {
			shown = items[:insightGroupPreviewCap]
			hidden = len(items) - insightGroupPreviewCap
		}
		groups = append(groups, insightGroup{
			Source:        source,
			Label:         labels[source],
			Count:         len(items),
			BarPct:        int(math.Round(float64(len(items)) / float64(maxCount) * 100)),
			HasStructural: hasStructural,
			Items:         items,
			Shown:         shown,
			Hidden:        hidden,
		})
	}

	// Groups with a proven finding rank first regardless of size — a repo's one
	// real dependency cycle matters more than its twenty-five heuristic god-class
	// candidates, and burying it under bigger buckets is exactly what made this
	// list feel like noise instead of guidance.
	sort.Slice(groups, func(i, j int) bool {
		if groups[i].HasStructural != groups[j].HasStructural {
			return groups[i].HasStructural
		}
		if groups[i].Count != groups[j].Count {
			return groups[i].Count > groups[j].Count
		}
		return groups[i].Label < groups[j].Label
	})

	return groups, structural, candidate
}
