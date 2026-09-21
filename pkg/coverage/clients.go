package coverage

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
)

// Client is one in-house client the config declares (clients:), accounted across every
// loaded repository: the members that carry it, the calls made through it, the routes
// those became, and the calls that did not, by cause.
//
// It sits beside the service report rather than inside it. A service's unresolved count
// says a call found no provider; a client's skipped count says a call never became a
// route at all, and a client with no call sites says the declaration itself matched
// nothing. Three different fixes, so three different numbers.
type Client struct {
	Spec      string         `json:"spec"`
	Receivers int            `json:"receivers"`
	CallSites int            `json:"call_sites"`
	Routes    int            `json:"routes"`
	Skipped   map[string]int `json:"skipped,omitempty"`
}

// SkippedTotal sums the client's skipped calls across causes.
func (c Client) SkippedTotal() int {
	n := 0
	for _, k := range c.Skipped {
		n += k
	}
	return n
}

// BuildClients sums the per-repository client accounts the extractors record (extraction
// facts carrying client_spec) into one entry per declared client, sorted by name. Empty
// when the config declares no clients.
func BuildClients(store *facts.Store) []Client {
	byName := map[string]*Client{}
	for _, f := range store.ByKind(facts.KindExtraction) {
		spec := f.PropString(facts.PropClientSpec)
		if spec == "" {
			continue
		}
		c := byName[spec]
		if c == nil {
			c = &Client{Spec: spec, Skipped: map[string]int{}}
			byName[spec] = c
		}
		c.Receivers += readInt(f.PropAny("receivers"))
		for _, e := range readEdgeCoverage(f) {
			c.CallSites += e.Detected
			c.Routes += e.Resolved
		}
		for cause, n := range parseSkipped(f.PropString("skipped")) {
			c.Skipped[cause] += n
		}
	}

	out := make([]Client, 0, len(byName))
	for _, c := range byName {
		if len(c.Skipped) == 0 {
			c.Skipped = nil
		}
		out = append(out, *c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Spec < out[j].Spec })
	return out
}

// parseSkipped reads the "cause=n,cause=n" form an account records its skipped calls in.
func parseSkipped(s string) map[string]int {
	out := map[string]int{}
	for _, part := range strings.Split(s, ",") {
		cause, n, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		if v, err := strconv.Atoi(n); err == nil {
			out[cause] += v
		}
	}
	return out
}

// skippedDetail renders skipped causes in a stable order.
func skippedDetail(c Client) string {
	causes := make([]string, 0, len(c.Skipped))
	for cause := range c.Skipped {
		causes = append(causes, cause)
	}
	sort.Strings(causes)
	parts := make([]string, 0, len(causes))
	for _, cause := range causes {
		parts = append(parts, fmt.Sprintf("%s ×%d", cause, c.Skipped[cause]))
	}
	return strings.Join(parts, ", ")
}

// RenderClientsText is the human-facing account of the declared clients, or "" when the
// config declares none.
func RenderClientsText(clients []Client) string {
	if len(clients) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("\nDeclared clients (clients: in the config)\n\n")

	width := len("client")
	for _, c := range clients {
		width = max(width, len(c.Spec))
	}
	width = min(width, 40)

	fmt.Fprintf(&sb, "  %-*s  %9s %10s %7s %8s\n", width, "client", "receivers", "call_sites", "routes", "skipped")
	for _, c := range clients {
		fmt.Fprintf(&sb, "  %-*s  %9d %10d %7d %8d\n",
			width, truncate(c.Spec, width), c.Receivers, c.CallSites, c.Routes, c.SkippedTotal())
	}

	for _, c := range clients {
		if c.SkippedTotal() > 0 {
			fmt.Fprintf(&sb, "\n%s skipped %s: %s\n", c.Spec,
				plural(c.SkippedTotal(), "call", "calls"), skippedDetail(c))
		}
	}
	for _, c := range clients {
		if c.CallSites > 0 {
			continue
		}
		if c.Receivers == 0 {
			fmt.Fprintf(&sb, "\n%s matched no call site: no class declares a member of its receiver_types.\n"+
				"Check them against the type a class injects.\n", c.Spec)
		} else {
			fmt.Fprintf(&sb, "\n%s matched no call site: %s of its receiver types exist, but none of its\n"+
				"declared methods is called on them. Check the method names.\n", c.Spec, plural(c.Receivers, "member", "members"))
		}
	}
	return sb.String()
}

// RenderClientsMarkdown is the agent-facing account of the declared clients, or "" when
// the config declares none.
func RenderClientsMarkdown(clients []Client) string {
	if len(clients) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("\n\n## Declared clients\n\n")
	sb.WriteString("In-house clients from `clients:` in the config. A client with no call sites matched nothing: " +
		"check its receiver_types (0 receivers) or its method names (receivers but no call sites).\n\n")
	sb.WriteString("| Client | Receivers | Call sites | Routes | Skipped |\n")
	sb.WriteString("|---|---|---|---|---|\n")
	for _, c := range clients {
		skipped := strconv.Itoa(c.SkippedTotal())
		if d := skippedDetail(c); d != "" {
			skipped += " (" + d + ")"
		}
		fmt.Fprintf(&sb, "| %s | %d | %d | %d | %s |\n", c.Spec, c.Receivers, c.CallSites, c.Routes, skipped)
	}
	return sb.String()
}
