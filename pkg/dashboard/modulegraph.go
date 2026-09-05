package dashboard

import (
	"fmt"
	"sort"
	"strings"

	"github.com/enola-labs/enola/internal/explainers/common"
	"github.com/enola-labs/enola/pkg/facts"
)

const (
	moduleGraphLimit         = 36
	moduleGraphOverviewLimit = 18
	moduleGraphOverviewEdges = 30
	moduleSearchLimit        = 500
)

type moduleNode struct {
	ID                        int
	Name, Display, File, Repo string
	Role                      string
	Members                   []string // non-nil only when Role == "cluster"
	Tooltip                   string
	X, Y                      int
	FanIn, FanOut             int
	Degree                    int
}

// MembersAttr renders Members for the "data-members" HTML attribute.
func (n moduleNode) MembersAttr() string {
	return strings.Join(n.Members, "|")
}

type moduleEdge struct {
	Source, Target         int
	X1, Y1, X2, Y2         int
	Curve                  bool // true when the target is not in a later column: a back or same-layer edge
	CX, CY                 int  // quadratic control point, set only when Curve is true
	SourceName, TargetName string
	SourceFile, TargetFile string
	Kind                   string
}

type moduleGraphView struct {
	Width, Height       int
	Nodes               []moduleNode
	Edges               []moduleEdge
	AllModules          []string
	AllModulesTruncated bool
	Total               int
	Limited             bool
	Focused             bool
	FocusName           string
	OmittedEdges        int
	ClusterCount        int
}

type moduleRaw struct {
	name, file, repo string
	out              map[string]bool
	in               int
}

// buildModuleGraph produces a deliberately bounded architecture map. It ranks
// production modules by connectedness, keeps the most structurally relevant
// ones, and renders only imports whose endpoints are both visible.
func buildModuleGraph(store *facts.Store) *moduleGraphView {
	return buildModuleGraphFocused(store, "")
}

func buildModuleGraphFocused(store *facts.Store, focus string) *moduleGraphView {
	if store == nil {
		return nil
	}
	mods := store.ByKind(facts.KindModule)
	if len(mods) == 0 {
		return nil
	}

	byName := make(map[string]*moduleRaw, len(mods))
	for _, m := range mods {
		if role, _ := m.Props[facts.PropModuleRole].(string); role == facts.ModuleRoleTest {
			continue
		}
		if _, exists := byName[m.Name]; !exists {
			byName[m.Name] = &moduleRaw{name: m.Name, file: m.File, repo: m.Repo, out: map[string]bool{}}
		}
	}
	for _, m := range mods {
		r := byName[m.Name]
		if r == nil {
			continue
		}
		// Snapshot stores publish a built graph whose module→module import edges are
		// synthesized from dependency facts. A small test/embedder store may not have
		// built that index yet, so fall back to relations carried directly by modules.
		type importEdge struct{ kind, target string }
		edges := make([]importEdge, 0)
		if graph := store.Graph(); graph != nil {
			for _, edge := range graph.ForwardEdges(m.Name) {
				edges = append(edges, importEdge{kind: edge.RelKind, target: edge.Target})
			}
		} else {
			for _, rel := range m.Relations {
				edges = append(edges, importEdge{kind: rel.Kind, target: rel.Target})
			}
		}
		for _, edge := range edges {
			if edge.kind != facts.RelImports || edge.target == m.Name || byName[edge.target] == nil || r.out[edge.target] {
				continue
			}
			r.out[edge.target] = true
			byName[edge.target].in++
		}
	}

	ranked := make([]*moduleRaw, 0, len(byName))
	for _, r := range byName {
		if r.in+len(r.out) > 0 {
			ranked = append(ranked, r)
		}
	}
	sort.Slice(ranked, func(i, j int) bool {
		di, dj := ranked[i].in+len(ranked[i].out), ranked[j].in+len(ranked[j].out)
		if di != dj {
			return di > dj
		}
		return ranked[i].name < ranked[j].name
	})
	if len(ranked) == 0 {
		return nil
	}
	total := len(ranked)
	searchable := ranked
	truncated := false
	if len(searchable) > moduleSearchLimit {
		searchable = searchable[:moduleSearchLimit]
		truncated = true
	}
	allModules := make([]string, 0, len(searchable))
	for _, r := range searchable {
		allModules = append(allModules, r.name)
	}
	focused := false
	if center := byName[focus]; focus != "" && center != nil {
		neighborhood := map[string]bool{focus: true}
		for target := range center.out {
			neighborhood[target] = true
		}
		for _, r := range ranked {
			if r.out[focus] {
				neighborhood[r.name] = true
			}
		}
		selected := []*moduleRaw{center}
		for _, r := range ranked {
			if r.name != focus && neighborhood[r.name] {
				selected = append(selected, r)
			}
		}
		ranked = selected
		focused = true
	}
	limit := moduleGraphOverviewLimit
	if focused {
		limit = moduleGraphLimit
	}
	if len(ranked) > limit {
		ranked = ranked[:limit]
	}

	const nodeW, nodeH, gapX, gapY, margin = 154, 36, 72, 34, 28
	layers := make(map[string]int, len(ranked))
	roles := make(map[string]string, len(ranked))
	var clusters map[string][]string  // representative name -> sorted member names
	var clusterOf map[string]string   // member name -> representative name
	var clusterStats map[string][2]int // representative name -> [external fan-in, external fan-out]
	if focused {
		for _, r := range ranked {
			switch {
			case r.name == focus:
				layers[r.name], roles[r.name] = 1, "selected"
			case r.out[focus]:
				layers[r.name], roles[r.name] = 0, "consumer"
			default:
				layers[r.name], roles[r.name] = 2, "dependency"
			}
		}
	} else {
		layers, clusters, clusterOf, clusterStats = condenseCycles(ranked)
		for _, r := range ranked {
			roles[r.name] = "module"
		}
	}
	resolve := func(name string) string {
		if id, ok := clusterOf[name]; ok {
			return id
		}
		return name
	}
	byRankedName := make(map[string]*moduleRaw, len(ranked))
	for _, r := range ranked {
		byRankedName[r.name] = r
	}
	byLayer := map[int][]string{}
	seen := map[string]bool{}
	maxLayer, maxRows := 0, 0
	for _, r := range ranked {
		id := resolve(r.name)
		if seen[id] {
			continue
		}
		seen[id] = true
		layer := layers[id]
		byLayer[layer] = append(byLayer[layer], id)
		if layer > maxLayer {
			maxLayer = layer
		}
	}
	for _, ids := range byLayer {
		if len(ids) > maxRows {
			maxRows = len(ids)
		}
	}
	view := &moduleGraphView{Width: margin*2 + (maxLayer+1)*nodeW + maxLayer*gapX, Height: margin*2 + maxRows*nodeH + (maxRows-1)*gapY, AllModules: allModules, AllModulesTruncated: truncated, Total: total, Limited: total > len(ranked), Focused: focused, FocusName: focus, ClusterCount: len(clusters)}
	index := make(map[string]int, len(seen))
	for layer := 0; layer <= maxLayer; layer++ {
		for row, id := range byLayer[layer] {
			i := len(view.Nodes)
			x := margin + layer*(nodeW+gapX)
			y := margin + row*(nodeH+gapY)
			node := moduleNode{ID: i, X: x, Y: y}
			if members, ok := clusters[id]; ok {
				stats := clusterStats[id]
				node.Name, node.Display, node.Role, node.Members = id, clusterLabel(id, members), "cluster", members
				node.Tooltip = clusterTooltip(members)
				node.FanIn, node.FanOut, node.Degree = stats[0], stats[1], stats[0]+stats[1]
			} else {
				r := byRankedName[id]
				node.Name, node.Display, node.File, node.Repo = r.name, moduleLabel(r.name), r.file, r.repo
				node.Role = roles[id]
				node.Tooltip = r.name
				node.FanIn, node.FanOut, node.Degree = r.in, len(r.out), r.in+len(r.out)
			}
			view.Nodes = append(view.Nodes, node)
			index[id] = i
		}
	}
	drawn := map[[2]int]bool{}
	for _, r := range ranked {
		srcID := resolve(r.name)
		for target := range r.out {
			tgtID := resolve(target)
			if srcID == tgtID {
				continue // intra-cluster edge; represented by the single node itself
			}
			i, ok := index[srcID]
			if !ok {
				continue
			}
			j, ok := index[tgtID]
			if !ok {
				continue
			}
			if drawn[[2]int{i, j}] {
				continue
			}
			drawn[[2]int{i, j}] = true
			a, b := view.Nodes[i], view.Nodes[j]
			edge := moduleEdge{Source: i, Target: j, SourceName: a.Name, TargetName: b.Name, SourceFile: a.File, TargetFile: b.File, Kind: facts.RelImports}
			if b.X > a.X {
				edge.X1, edge.Y1 = a.X+nodeW, a.Y+nodeH/2
				edge.X2, edge.Y2 = b.X, b.Y+nodeH/2
			} else {
				// b is in the same column as a, or an earlier one (a cycle, or a
				// forward-ranked node pointing back into one). A straight line from
				// a's right edge to b's left edge would cross every node stacked
				// between them, so loop it out past both nodes' right edges instead.
				edge.Curve = true
				edge.X1, edge.Y1 = a.X+nodeW, a.Y+nodeH/2
				edge.X2, edge.Y2 = b.X+nodeW, b.Y+nodeH/2
				rim := a.X + nodeW
				if b.X+nodeW > rim {
					rim = b.X + nodeW
				}
				dy := edge.Y2 - edge.Y1
				if dy < 0 {
					dy = -dy
				}
				// Nest the arcs by vertical span, like an arc diagram: edges spanning
				// many rows bow further out than edges between neighbors. A fixed
				// bow made every back-edge in a tangled column converge on the same
				// curve, reading as one thick cable instead of distinct edges.
				bow := 20 + dy/3
				if max := nodeW + gapX/2; bow > max {
					bow = max
				}
				edge.CX = rim + bow
				edge.CY = (edge.Y1 + edge.Y2) / 2
			}
			if edge.CX+margin > view.Width {
				view.Width = edge.CX + margin
			}
			view.Edges = append(view.Edges, edge)
		}
	}
	sortEdgesByEndpoints(view.Edges)
	if !focused && len(view.Edges) > moduleGraphOverviewEdges {
		view.OmittedEdges = len(view.Edges) - moduleGraphOverviewEdges
		// Keep the edges between the most-connected endpoints rather than an
		// arbitrary source/target-ordered prefix, so the truncated overview still
		// shows the structurally significant dependency backbone.
		sort.SliceStable(view.Edges, func(i, j int) bool {
			di := view.Nodes[view.Edges[i].Source].Degree + view.Nodes[view.Edges[i].Target].Degree
			dj := view.Nodes[view.Edges[j].Source].Degree + view.Nodes[view.Edges[j].Target].Degree
			return di > dj
		})
		view.Edges = view.Edges[:moduleGraphOverviewEdges]
		sortEdgesByEndpoints(view.Edges)
	}
	// Paint wide arcs first so the tighter arcs and straight lines nested inside
	// them (CX is 0 on straight edges) land on top and stay readable.
	sort.SliceStable(view.Edges, func(i, j int) bool { return view.Edges[i].CX > view.Edges[j].CX })
	return view
}

func sortEdgesByEndpoints(edges []moduleEdge) {
	sort.Slice(edges, func(i, j int) bool {
		if edges[i].Source != edges[j].Source {
			return edges[i].Source < edges[j].Source
		}
		return edges[i].Target < edges[j].Target
	})
}

// dependencyLayers places consumers before the modules they depend on. Kahn's
// algorithm gives acyclic regions stable layers; cycle members remain together
// in the first layer, which truthfully exposes that no ordering exists for them.
func dependencyLayers(ranked []*moduleRaw) map[string]int {
	layers := make(map[string]int, len(ranked))
	indegree := make(map[string]int, len(ranked))
	visible := make(map[string]bool, len(ranked))
	for _, r := range ranked {
		visible[r.name] = true
	}
	for _, r := range ranked {
		for target := range r.out {
			if visible[target] {
				indegree[target]++
			}
		}
	}
	queue := make([]string, 0, len(ranked))
	for _, r := range ranked {
		if indegree[r.name] == 0 {
			queue = append(queue, r.name)
		}
	}
	byName := make(map[string]*moduleRaw, len(ranked))
	for _, r := range ranked {
		byName[r.name] = r
	}
	for len(queue) > 0 {
		name := queue[0]
		queue = queue[1:]
		for target := range byName[name].out {
			if !visible[target] {
				continue
			}
			if layers[target] < layers[name]+1 {
				layers[target] = layers[name] + 1
			}
			indegree[target]--
			if indegree[target] == 0 {
				queue = append(queue, target)
			}
		}
	}
	return layers
}

// condenseCycles collapses each strongly-connected component of more than one
// module into a single supernode named after its most-connected member, so
// the graph handed to dependencyLayers is always acyclic — the condensation of
// any directed graph has no cycles, so every resulting layer edge is strictly
// forward. Returns per-id layers (real module names for unclustered modules,
// the representative's name for a cluster), the cluster's member lists keyed
// by representative, the member->representative mapping, and each cluster's
// external [fan-in, fan-out] counts (edges to/from modules outside it, after
// resolving cluster membership — not the representative's own raw in/out,
// which would still count the now-hidden intra-cluster edges).
func condenseCycles(ranked []*moduleRaw) (layers map[string]int, clusters map[string][]string, clusterOf map[string]string, stats map[string][2]int) {
	visible := make(map[string]bool, len(ranked))
	byName := make(map[string]*moduleRaw, len(ranked))
	for _, r := range ranked {
		visible[r.name] = true
		byName[r.name] = r
	}
	graph := make(map[string][]string, len(ranked))
	for _, r := range ranked {
		var out []string
		for target := range r.out {
			if visible[target] && target != r.name {
				out = append(out, target)
			}
		}
		graph[r.name] = out
	}

	clusterOf = make(map[string]string)
	clusters = make(map[string][]string)
	for _, members := range common.StronglyConnectedComponents(graph) {
		if len(members) < 2 {
			continue
		}
		sorted := append([]string(nil), members...)
		sort.Strings(sorted)
		rep, repDegree := sorted[0], -1
		for _, m := range sorted {
			if d := byName[m].in + len(byName[m].out); d > repDegree || (d == repDegree && m < rep) {
				repDegree, rep = d, m
			}
		}
		clusters[rep] = sorted
		for _, m := range sorted {
			clusterOf[m] = rep
		}
	}

	resolve := func(name string) string {
		if id, ok := clusterOf[name]; ok {
			return id
		}
		return name
	}
	condensed := make(map[string]*moduleRaw, len(ranked))
	for _, r := range ranked {
		id := resolve(r.name)
		c, ok := condensed[id]
		if !ok {
			c = &moduleRaw{name: id, out: map[string]bool{}}
			condensed[id] = c
		}
		for target := range r.out {
			if !visible[target] {
				continue
			}
			if rt := resolve(target); rt != id {
				c.out[rt] = true
			}
		}
	}
	condensedRanked := make([]*moduleRaw, 0, len(condensed))
	for _, c := range condensed {
		condensedRanked = append(condensedRanked, c)
	}
	sort.Slice(condensedRanked, func(i, j int) bool { return condensedRanked[i].name < condensedRanked[j].name })

	stats = make(map[string][2]int, len(clusters))
	if len(clusters) > 0 {
		fanIn := make(map[string]int, len(condensed))
		for _, c := range condensed {
			for target := range c.out {
				fanIn[target]++
			}
		}
		for rep := range clusters {
			stats[rep] = [2]int{fanIn[rep], len(condensed[rep].out)}
		}
	}
	return dependencyLayers(condensedRanked), clusters, clusterOf, stats
}

// clusterLabel formats the display text for a collapsed cyclic cluster, named
// after its most-connected member with a count of the others folded into it.
// The ⟲ glyph is a self-explanatory visual flag: dashed borders alone are easy
// to miss or forget the meaning of at a glance.
func clusterLabel(rep string, members []string) string {
	return fmt.Sprintf("⟲ %s +%d", moduleLabel(rep), len(members)-1)
}

// clusterTooltip is the hover title for a cluster node — it names every
// member up front, since the collapsed label alone only names one of them.
func clusterTooltip(members []string) string {
	return fmt.Sprintf("%d modules import each other in a cycle: %s\nClick to see them, or click a name to explore just that one.", len(members), strings.Join(members, ", "))
}

func moduleLabel(name string) string {
	const limit = 25
	if len(name) <= limit {
		return name
	}
	parts := strings.Split(name, "/")
	if len(parts) > 1 {
		tail := parts[len(parts)-2] + "/" + parts[len(parts)-1]
		if len(tail) <= limit {
			return tail
		}
	}
	return "…" + name[len(name)-(limit-1):]
}
