package check

import (
	"fmt"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/internal/metrics"
)

// AttachPackageMetrics reports what the change did to the Martin package metrics.
// Reported, never graded: see metrics.Delta.
//
// baseStore and currentStore are the whole graphs. On a partial verdict they are not
// what was graded, so both sides are rebuilt from the facts of the producers they
// share: a language whose extractor ran on one side only would otherwise read as
// every one of its packages added or removed.
func AttachPackageMetrics(v Verdict, baseStore, currentStore *facts.Store, base, current *facts.Snapshot, owners FileOwnership, focus string) Verdict {
	if baseStore == nil || currentStore == nil {
		return v
	}
	switch v.Status {
	case StatusClean, StatusRegression, StatusPartialClean, StatusPartialRegression:
	default:
		return v
	}
	if v.Intersection != nil {
		b, c, ok := sharedProducerStores(v.Intersection, base, current, owners)
		if !ok {
			return v
		}
		baseStore, currentStore = b, c
	}
	d := metrics.ComputeDelta(baseStore, currentStore, metrics.FocusFilter(focus))
	v.PackageMetrics = &d
	return v
}

// sharedProducerStores filters both snapshots exactly as RegradeIntersection did. The
// producers are copied because excludeSide counts into them, and the verdict already
// reports those counts once.
func sharedProducerStores(g *IntersectionGrading, base, current *facts.Snapshot, owners FileOwnership) (*facts.Store, *facts.Store, bool) {
	if base == nil || current == nil {
		return nil, nil, false
	}
	excluded := append([]ExcludedProducer(nil), g.Excluded...)
	matchers := make([]*producerMatcher, 0, len(excluded))
	for i := range excluded {
		m, err := newProducerMatcher(&excluded[i], owners)
		if err != nil {
			return nil, nil, false
		}
		matchers = append(matchers, m)
	}
	b, c := facts.NewStore(), facts.NewStore()
	b.Add(excludeSide(base, matchers, SideBaseline).Facts...)
	c.Add(excludeSide(current, matchers, SideCurrent).Facts...)
	return b, c, true
}

// writePackageMetrics prints the packages whose metrics moved. Silent when none did:
// unchanged aggregates over an untouched population say nothing about the change.
// Text and JSON only, for the reason writeReviewers gives.
func (v Verdict) writePackageMetrics(sb *strings.Builder) {
	d := v.PackageMetrics
	if d == nil || len(d.Packages) == 0 {
		return
	}
	fmt.Fprintf(sb, "\nPackage metrics (%d moved; %d worse, %d better) — reported, never graded:\n",
		len(d.Packages), d.Worsened, d.Improved)
	fmt.Fprintf(sb, "  avg D %s, avg I %s, off main sequence %s\n",
		moved2(d.Before.AvgD, d.After.AvgD), moved2(d.Before.AvgI, d.After.AvgI), movedInt(d.Before.OffMain, d.After.OffMain))
	for i, r := range d.Packages {
		if i == listCap {
			fmt.Fprintf(sb, "  … %d more package(s)\n", len(d.Packages)-listCap)
			break
		}
		fmt.Fprintf(sb, "  %s  %s\n", r.Package, packageMetricLine(r))
	}
}

func packageMetricLine(r metrics.DeltaRow) string {
	switch {
	case r.Before == nil:
		a := r.After
		return fmt.Sprintf("added: Ca %d  Ce %d  I %.2f  A %.2f  D %.2f  (%s)", a.Ca, a.Ce, a.Instability, a.Abstractness, a.Distance, r.ZoneAfter)
	case r.After == nil:
		b := r.Before
		return fmt.Sprintf("removed: Ca %d  Ce %d  I %.2f  A %.2f  D %.2f  (%s)", b.Ca, b.Ce, b.Instability, b.Abstractness, b.Distance, r.ZoneBefore)
	}
	b, a := r.Before, r.After
	line := fmt.Sprintf("Ca %s  Ce %s  I %s  A %s  D %s",
		movedInt(b.Ca, a.Ca), movedInt(b.Ce, a.Ce), moved2(b.Instability, a.Instability),
		moved2(b.Abstractness, a.Abstractness), moved2(b.Distance, a.Distance))
	switch {
	case r.ZoneIncidental:
		line += fmt.Sprintf("  (%s → %s: the population's rigid floor moved, not this package)", r.ZoneBefore, r.ZoneAfter)
	case r.ZoneBefore != r.ZoneAfter:
		line += fmt.Sprintf("  (%s → %s)", r.ZoneBefore, r.ZoneAfter)
	}
	return line
}

func movedInt(b, a int) string {
	if b == a {
		return fmt.Sprintf("%d", a)
	}
	return fmt.Sprintf("%d→%d", b, a)
}

func moved2(b, a float64) string {
	if fmt.Sprintf("%.2f", b) == fmt.Sprintf("%.2f", a) {
		return fmt.Sprintf("%.2f", a)
	}
	return fmt.Sprintf("%.2f→%.2f", b, a)
}
