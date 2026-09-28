package metrics

import (
	"math"
	"sort"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
)

// Delta is what a change did to the package metrics: the population aggregates on
// both sides and one row per package whose measurements moved. It is reported, never
// graded. D on a small package is noisy (one interface added to a two-type package
// moves A by half), and a new importer elsewhere shifts Ca on a package the change
// never touched, so a threshold on it would need tuning nobody has done yet.
type Delta struct {
	Before DeltaAggregate `json:"before"`
	After  DeltaAggregate `json:"after"`
	// Worsened and Improved count the rows present on both sides whose D moved
	// away from or towards the main sequence. Incidental rows are in neither.
	Worsened int        `json:"worsened"`
	Improved int        `json:"improved"`
	Packages []DeltaRow `json:"packages,omitempty"`
}

// DeltaAggregate is the whole population's health on one side. It is never narrowed
// by a focus: an average over whatever a substring happened to match is not a number
// anybody can compare with anything.
type DeltaAggregate struct {
	Analyzed int     `json:"analyzed"`
	AvgI     float64 `json:"avg_instability"`
	AvgD     float64 `json:"avg_distance"`
	OffMain  int     `json:"off_main_sequence"`
}

// DeltaRow is one package that moved. Before is nil for a package the change added,
// After for one it removed.
type DeltaRow struct {
	Package    string         `json:"package"`
	Repo       string         `json:"repo,omitempty"`
	Before     *PackageMetric `json:"before,omitempty"`
	After      *PackageMetric `json:"after,omitempty"`
	ZoneBefore Zone           `json:"zone_before,omitempty"`
	ZoneAfter  Zone           `json:"zone_after,omitempty"`
	// ZoneIncidental marks a zone flip with every measurement of the package itself
	// unchanged. The rigid gate is the population's 90th-percentile Ca, so another
	// package gaining dependents can move this one across it; that is not something
	// the change did to this package.
	ZoneIncidental bool `json:"zone_incidental,omitempty"`
}

// DeltaD is the signed change in distance. A missing side counts as absent rather
// than as zero, so an added package reports its own D.
func (r DeltaRow) DeltaD() float64 {
	var before, after float64
	if r.Before != nil {
		before = r.Before.Distance
	}
	if r.After != nil {
		after = r.After.Distance
	}
	return round3(after - before)
}

// ComputeDelta scores both stores with the same collect/compute core as the
// package_metrics tool and reports what moved. keep narrows the rows (never the
// aggregates); nil keeps everything.
func ComputeDelta(base, current *facts.Store, keep func(pkg string) bool) Delta {
	before := Compute(base)
	after := Compute(current)
	return Delta{
		Before: summarizeSide(before),
		After:  summarizeSide(after),
	}.withRows(diffMetrics(before, after), keep)
}

func summarizeSide(ms []PackageMetric) DeltaAggregate {
	agg := aggregateMetrics(ms)
	return DeltaAggregate{Analyzed: agg.analyzed, AvgI: agg.avgI, AvgD: agg.avgD, OffMain: agg.offMain}
}

func (d Delta) withRows(rows []DeltaRow, keep func(pkg string) bool) Delta {
	for _, r := range rows {
		if keep != nil && !keep(r.Package) {
			continue
		}
		d.Packages = append(d.Packages, r)
		if r.Before == nil || r.After == nil || r.ZoneIncidental {
			continue
		}
		switch dd := r.DeltaD(); {
		case dd > 0:
			d.Worsened++
		case dd < 0:
			d.Improved++
		}
	}
	return d
}

// diffMetrics pairs packages by repo and name and returns the ones that moved,
// off-main-sequence zone changes first, then by the size of the D change. Packages with no types on
// either side are left out: A and D are undefined for them, which is why the
// aggregates leave them out too.
func diffMetrics(before, after []PackageMetric) []DeltaRow {
	key := func(m PackageMetric) string { return m.Repo + "\x00" + m.Package }
	byKey := make(map[string]*PackageMetric, len(before))
	for i := range before {
		byKey[key(before[i])] = &before[i]
	}
	var rows []DeltaRow
	seen := make(map[string]bool, len(after))
	for i := range after {
		a := &after[i]
		k := key(*a)
		seen[k] = true
		if row, ok := deltaRow(byKey[k], a); ok {
			rows = append(rows, row)
		}
	}
	for i := range before {
		if b := &before[i]; !seen[key(*b)] {
			if row, ok := deltaRow(b, nil); ok {
				rows = append(rows, row)
			}
		}
	}
	sort.SliceStable(rows, func(i, j int) bool {
		zi, zj := rows[i].zoneMoved(), rows[j].zoneMoved()
		if zi != zj {
			return zi
		}
		di, dj := math.Abs(rows[i].DeltaD()), math.Abs(rows[j].DeltaD())
		if di != dj {
			return di > dj
		}
		if rows[i].Package != rows[j].Package {
			return rows[i].Package < rows[j].Package
		}
		return rows[i].Repo < rows[j].Repo
	})
	return rows
}

func deltaRow(b, a *PackageMetric) (DeltaRow, bool) {
	typed := func(m *PackageMetric) bool { return m != nil && m.ClassesInterfaces > 0 }
	if !typed(b) && !typed(a) {
		return DeltaRow{}, false
	}
	row := DeltaRow{Before: b, After: a}
	if a != nil {
		row.Package, row.Repo = a.Package, a.Repo
		row.ZoneAfter = Classify(*a)
	} else {
		row.Package, row.Repo = b.Package, b.Repo
	}
	if b != nil {
		row.ZoneBefore = Classify(*b)
	}
	if b == nil || a == nil {
		return row, true
	}
	same := sameMeasurements(*b, *a)
	if same && row.ZoneBefore == row.ZoneAfter {
		return DeltaRow{}, false
	}
	row.ZoneIncidental = same
	return row, true
}

// sameMeasurements compares everything measured about the package itself, and
// deliberately not RigidCaFloor, which belongs to the population.
func sameMeasurements(b, a PackageMetric) bool {
	return b.ClassesInterfaces == a.ClassesInterfaces && b.Ca == a.Ca && b.Ce == a.Ce &&
		b.Instability == a.Instability && b.Abstractness == a.Abstractness &&
		b.Distance == a.Distance && b.DataHolderRatio == a.DataHolderRatio
}

// zoneMoved is a package entering or leaving an off-main-sequence corner. A move
// between main-sequence and neutral is a presentation band, not a finding, so it
// ranks by its D change like any other row.
func (r DeltaRow) zoneMoved() bool {
	offMain := func(z Zone) bool { return z == ZonePain || z == ZoneUseless }
	return r.Before != nil && r.After != nil && !r.ZoneIncidental &&
		r.ZoneBefore != r.ZoneAfter && (offMain(r.ZoneBefore) || offMain(r.ZoneAfter))
}

// FocusFilter is the keep function for a --focus value: the same case-insensitive
// substring the snapshot diff narrows by.
func FocusFilter(focus string) func(pkg string) bool {
	focus = strings.ToLower(strings.TrimSpace(focus))
	if focus == "" {
		return nil
	}
	return func(pkg string) bool { return strings.Contains(strings.ToLower(pkg), focus) }
}
