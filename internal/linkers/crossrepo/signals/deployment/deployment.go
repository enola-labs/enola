// Package deployment links a deployment repository to the code repositories its
// releases run.
package deployment

import (
	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/internal/linkers/binders/deployedcode"
	"github.com/enola-labs/enola/pkg/plugin"
)

// Signal turns each Application's deploys_repo, which the deployed-code binder sets,
// into a cross-repo edge from the repository holding the Application to the one whose
// code it runs, sampled with the Applications that say so. A GitOps change and a code
// change then meet at the service graph: what deploys this repository, and what a
// deployment repository's change reaches.
//
// It reports no coverage: most releases in a GitOps repository deploy software no
// loaded repository holds (a mesh, a secrets agent), and that is not a blind spot.
type Signal struct{}

// New returns the signal.
func New() *Signal { return &Signal{} }

func (s *Signal) Name() string { return "deployment" }

func (s *Signal) Phase() plugin.SignalPhase { return plugin.PhaseDirectional }

func (s *Signal) Contribute(in plugin.SignalInput, out plugin.EvidenceSink) {
	for _, f := range in.Facts() {
		if f.Kind != facts.KindSymbol || f.Repo == "" || f.PropString("k8s_kind") != "Application" {
			continue
		}
		target := f.PropString(deployedcode.PropDeploysRepo)
		if target == "" || target == f.Repo {
			continue
		}
		e := out.Edge(f.Repo, target)
		e.Via(facts.ViaDeployment)
		e.Sample(plugin.BucketDeployments, f.Name)
	}
}
