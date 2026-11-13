// Package deployedcode names the code repository an Argo CD Application deploys.
package deployedcode

import (
	"context"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/pkg/plugin"
)

// PropDeploysRepo is the prop naming the loaded repository whose code a release runs.
const PropDeploysRepo = "deploys_repo"

// Binder sets deploys_repo on every Application whose release runs the code of
// another loaded repository.
//
// A release runs images, and an image is named for what it is built from: its path
// below the registry host (`registry.example.com/shop-web/app:1.2`) names the
// repository more often than not, and a GitOps convention names the namespace after
// the service. The extractor knows the image and the namespace but not which
// repositories are loaded, so the join is a binder's.
//
// An image path segment naming a loaded repository answers, the registry host
// skipped; when an Application's images name several, its destination namespace
// must name one of them, or it names none. With no image naming one, the
// destination namespace answers, then the names it serves. Labels compare the way
// cross-repo linking compares them: case, '-' and '_' folded. The Application's own
// repository never answers.
type Binder struct{}

// New returns the binder.
func New() *Binder { return &Binder{} }

// Name identifies the binder.
func (b *Binder) Name() string { return "deployed-code" }

// Stage is pre-link: the deployment signal reads the prop during linking.
func (b *Binder) Stage() plugin.BindStage { return plugin.StagePreLink }

// Bind sets the prop.
func (b *Binder) Bind(_ context.Context, store *facts.Store) error {
	labels := map[string]string{}
	for _, l := range store.RepoLabels() {
		labels[facts.NormalizeRepoLabel(l)] = l
	}
	if len(labels) < 2 {
		return nil
	}
	store.UpdateWhere(func(f *facts.Fact) {
		if f.Kind != facts.KindSymbol || f.PropString("k8s_kind") != "Application" || f.PropString("language") != "kubernetes" {
			return
		}
		lookup := func(s string) string {
			if l := labels[facts.NormalizeRepoLabel(s)]; l != "" && l != f.Repo {
				return l
			}
			return ""
		}
		repo := DeployedRepo(f, lookup)
		if repo == "" {
			delete(f.Props, PropDeploysRepo)
			return
		}
		f.SetProp(PropDeploysRepo, repo)
	})
	return nil
}

// DeployedRepo decides the repository an Application deploys, with lookup mapping a
// candidate name to a loaded repository other than its own, or "".
func DeployedRepo(f *facts.Fact, lookup func(string) string) string {
	ns := lookup(f.PropString("destination_namespace"))
	found := map[string]bool{}
	for _, img := range stringList(f.PropAny("images")) {
		for _, seg := range imageSegments(img) {
			if r := lookup(seg); r != "" {
				found[r] = true
				break
			}
		}
	}
	switch {
	case len(found) == 1:
		for r := range found {
			return r
		}
	case len(found) > 1:
		if found[ns] {
			return ns
		}
		return ""
	}
	if ns != "" {
		return ns
	}
	for _, s := range stringList(f.PropAny("serves")) {
		if r := lookup(s); r != "" {
			return r
		}
	}
	return ""
}

// imageSegments lists an image's path segments below its registry host, tag and
// digest dropped: registry.example.com:5000/team/web:1.2 is [team web].
func imageSegments(img string) []string {
	if i := strings.Index(img, "@"); i >= 0 {
		img = img[:i]
	}
	if slash, colon := strings.LastIndex(img, "/"), strings.LastIndex(img, ":"); colon > slash {
		img = img[:colon]
	}
	segs := strings.Split(img, "/")
	if len(segs) > 1 && (strings.ContainsAny(segs[0], ".:") || segs[0] == "localhost") {
		segs = segs[1:]
	}
	return segs
}

func stringList(v any) []string {
	switch x := v.(type) {
	case []string:
		return x
	case []any:
		out := make([]string, 0, len(x))
		for _, e := range x {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}
