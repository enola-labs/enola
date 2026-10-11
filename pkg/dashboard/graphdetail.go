package dashboard

import (
	"sort"

	"github.com/enola-labs/enola/internal/facts"
)

// serviceRow is one entry in the Services modal: a service node (a whole repo in
// the cross-repo "graph of graphs") and how many other services it depends on.
type serviceRow struct {
	Name      string
	DependsOn int
}

// edgeRow is one entry in the Cross-repo edges modal: a consumer service that
// depends on a provider service.
type edgeRow struct {
	Consumer string
	Provider string
}

// graphDetails enumerates the service and cross-repo-edge lists from the live
// fact store — the same source the OSS engine reduces to the receipt's
// service_count / cross_repo_edge_count. Services are the KindService nodes; each
// cross-repo edge is a service node's depends_on relation (consumer → provider).
// A nil store (no snapshot loaded, or a test fake) yields empty lists, so the
// dashboard cards fall back to plain, non-clickable numbers. Store.ByKind is
// concurrency-safe and returns fact copies, so this only ever reads.
func graphDetails(store *facts.Store) (services []serviceRow, edges []edgeRow) {
	if store == nil {
		return nil, nil
	}

	for _, svc := range store.ByKind(facts.KindService) {
		dependsOn := 0
		for _, rel := range svc.Relations {
			if rel.Kind == facts.RelDependsOn {
				dependsOn++
				edges = append(edges, edgeRow{Consumer: svc.Name, Provider: rel.Target})
			}
		}
		services = append(services, serviceRow{Name: svc.Name, DependsOn: dependsOn})
	}

	// Stable output, mirroring the OSS receipt's sort-by-label convention.
	sort.Slice(services, func(i, j int) bool { return services[i].Name < services[j].Name })
	sort.Slice(edges, func(i, j int) bool {
		if edges[i].Consumer != edges[j].Consumer {
			return edges[i].Consumer < edges[j].Consumer
		}
		return edges[i].Provider < edges[j].Provider
	})

	return services, edges
}
