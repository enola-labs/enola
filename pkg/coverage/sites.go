package coverage

import (
	"fmt"
	"sort"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/internal/linkers/binders/unmatchedroutes"
)

type CallSite struct {
	Method string `json:"method,omitempty"`
	Path   string `json:"path"`
	File   string `json:"file"`
	Line   int    `json:"line,omitempty"`
	Reason string `json:"reason,omitempty"`
}

func (r Report) AttachUnresolvedSites(store *facts.Store) {
	byRepo := make(map[string][]CallSite)
	for _, f := range store.ByKind(facts.KindRoute) {
		if f.PropString(facts.PropRole) != facts.RoleClient {
			continue
		}
		if b, _ := f.PropAny(unmatchedroutes.PropUnmatchedByServer).(bool); !b {
			continue
		}
		if b, _ := f.PropAny("test_double").(bool); b {
			continue
		}
		byRepo[f.Repo] = append(byRepo[f.Repo], CallSite{
			Method: f.PropString("method"),
			Path:   f.Name,
			File:   f.File,
			Line:   f.Line,
			Reason: f.PropString(unmatchedroutes.PropUnmatchedReason),
		})
	}
	for i := range r {
		sites := byRepo[r[i].Service]
		sort.Slice(sites, func(a, b int) bool {
			if sites[a].File != sites[b].File {
				return sites[a].File < sites[b].File
			}
			return sites[a].Line < sites[b].Line
		})
		r[i].UnresolvedSites = sites
	}
}

func (r Report) RenderUnresolvedSites() string {
	var sb strings.Builder
	for _, s := range r {
		if len(s.UnresolvedSites) == 0 {
			continue
		}
		type group struct {
			key    string
			reason string
			where  []string
		}
		var order []string
		groups := make(map[string]*group)
		for _, c := range s.UnresolvedSites {
			key := strings.TrimSpace(c.Method + " " + c.Path)
			g := groups[key]
			if g == nil {
				g = &group{key: key, reason: c.Reason}
				groups[key] = g
				order = append(order, key)
			}
			g.where = append(g.where, fmt.Sprintf("%s:%d", c.File, c.Line))
		}
		fmt.Fprintf(&sb, "\n%s — %s:\n", s.Service, plural(len(s.UnresolvedSites), "unresolved call site", "unresolved call sites"))
		for _, key := range order {
			g := groups[key]
			reason := ""
			if g.reason != "" {
				reason = "  (" + g.reason + ")"
			}
			fmt.Fprintf(&sb, "  %s%s\n", g.key, reason)
			for _, w := range g.where {
				fmt.Fprintf(&sb, "      %s\n", w)
			}
		}
	}
	return sb.String()
}
