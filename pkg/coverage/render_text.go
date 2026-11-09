package coverage

import (
	"fmt"
	"sort"
	"strings"
)

// unresolvedSampleCap bounds the per-service list of edge types with unresolved call
// sites. Long enough to be diagnostic, short enough that a wide graph stays readable.
const unresolvedSampleCap = 5

// RenderText is the human-facing view.
//
// Deliberately not the markdown table: a pipe table wider than the terminal wraps into
// noise, and a person reading this is asking a different question from an agent. An
// agent wants the whole matrix; a person wants to know whether to believe the
// cross-repo edges, which means leading with what did NOT resolve.
func (r Report) RenderText() string {
	if len(r) == 0 {
		return noServicesMessage()
	}

	var sb strings.Builder
	sb.WriteString("Cross-repo edge coverage\n\n")

	width := 7 // len("service")
	for _, s := range r {
		if len(s.Service) > width {
			width = len(s.Service)
		}
	}
	if width > 40 {
		width = 40
	}

	fmt.Fprintf(&sb, "  %-*s  %-13s %9s %9s %11s\n", width, "service", "classification", "detected", "resolved", "unresolved")
	for _, s := range r {
		fmt.Fprintf(&sb, "  %-*s  %-13s %9d %9d %11d\n",
			width, truncate(s.Service, width), s.Classification, s.Detected(), s.Resolved(), s.UnresolvedTotal)
	}

	r.writeUnresolved(&sb)
	r.writeVerdict(&sb)
	return sb.String()
}

// writeUnresolved lists what did not resolve, per service.
//
// This is not optional and is not behind a flag. Cross-repo resolution is the claim
// hardest to check from outside, and a report that showed only successes would be
// marketing — the misses are what make the successes worth believing, and they are
// also the actionable part: an unresolved outbound call site is either a blind spot in
// enola or a dependency the reader did not know they had.
func (r Report) writeUnresolved(sb *strings.Builder) {
	type miss struct {
		service string
		kinds   []string
		total   int
	}
	var misses []miss
	for _, s := range r {
		if s.UnresolvedTotal == 0 && s.OutsideTotal == 0 {
			continue
		}
		m := miss{service: s.Service, total: s.UnresolvedTotal + s.OutsideTotal}
		for _, c := range s.EdgeCoverage {
			if c.Unresolved > 0 {
				m.kinds = append(m.kinds, fmt.Sprintf("%s ×%d", c.EdgeType, c.Unresolved))
			}
			if c.Outside > 0 {
				m.kinds = append(m.kinds, fmt.Sprintf("%s ×%d to a service not loaded", c.EdgeType, c.Outside))
			}
		}
		sort.Strings(m.kinds)
		misses = append(misses, m)
	}
	if len(misses) == 0 {
		sb.WriteString("\nEvery detected outbound call site resolved to a loaded service.\n")
		return
	}

	total, outside := 0, false
	for _, m := range misses {
		total += m.total
	}
	for _, s := range r {
		outside = outside || s.OutsideTotal > 0
	}
	// The heading counts both buckets, so it names both once a call to an unloaded
	// service is among them: those read 0 in the Unresolved column.
	heading := "Unresolved outbound call sites"
	if outside {
		heading = "Outbound call sites unresolved or to a service not loaded"
	}
	fmt.Fprintf(sb, "\n%s (%d across %s):\n",
		heading, total, plural(len(misses), "service", "services"))
	for _, m := range misses {
		kinds := m.kinds
		if len(kinds) > unresolvedSampleCap {
			kinds = append(kinds[:unresolvedSampleCap], fmt.Sprintf("… %d more", len(m.kinds)-unresolvedSampleCap))
		}
		fmt.Fprintf(sb, "  %-28s %s\n", truncate(m.service, 28), strings.Join(kinds, ", "))
	}
	sb.WriteString("\nEach is a call enola detected but could not point at a loaded service — either a\n" +
		"repository you have not snapshotted, a third-party endpoint, or a blind spot in\n" +
		"enola's extraction. Load the missing repository and re-run to tell them apart.\n")
}

// writeVerdict closes with the distinction the report exists to make.
func (r Report) writeVerdict(sb *strings.Builder) {
	gaps := r.Gaps()
	if gaps == 0 {
		return
	}
	fmt.Fprintf(sb, "\n%s classified `coverage_gap`: no resolved outbound edges, but enola DID\ndetect outbound call sites. Do not read these as isolated — they are the case where\n\"depends on nothing\" and \"enola could not tell\" look identical from the graph alone.\n",
		plural(gaps, "service is", "services are"))
}

// noServicesMessage explains an empty report rather than printing an empty table.
// Running this against a single repository is the most likely first attempt, and a
// blank table there reads as a broken tool rather than as a missing second repo.
func noServicesMessage() string {
	return "No services in this graph, so there are no cross-repo edges to report on.\n\n" +
		"Coverage compares repositories against each other, so it needs at least two loaded\n" +
		"in one graph. Index a cluster and re-run:\n\n" +
		"    enola --generate ci/cluster.yaml   # a config listing several repos under `repos:`\n" +
		"    enola coverage ci/cluster.yaml\n"
}

func truncate(s string, max int) string {
	if len(s) <= max || max <= 1 {
		return s
	}
	return "…" + s[len(s)-(max-1):]
}

func plural(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}
