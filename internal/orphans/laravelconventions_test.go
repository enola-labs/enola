package orphans

import (
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// TestLaravelPoliciesAreDiscoveredForTheirModel: the gate finds the policy of a
// model by name, in a Policies namespace beside the model or above it. A policy
// with no model of its name, a class named like a policy outside a Policies
// namespace, and a policy for a class that is no model are still reported.
func TestLaravelPoliciesAreDiscoveredForTheirModel(t *testing.T) {
	var s laravelStore
	s.class(`App\Models\Song`, "app/Models/Song.php", implements(laravelModel))
	s.class(`App\Models\Billing\Invoice`, "app/Models/Billing/Invoice.php", implements(laravelModel))
	s.class(`App\Services\Mixer`, "app/Services/Mixer.php")
	s.class(`App\Policies\SongPolicy`, "app/Policies/SongPolicy.php")
	s.class(`App\Models\Billing\Policies\InvoicePolicy`, "app/Models/Billing/Policies/InvoicePolicy.php")
	s.class(`App\Policies\GhostPolicy`, "app/Policies/GhostPolicy.php")
	s.class(`App\Policies\MixerPolicy`, "app/Policies/MixerPolicy.php")
	s.class(`App\Support\SongPolicy`, "app/Support/SongPolicy.php")
	s.class(`Other\Policies\SongPolicy`, "other/Policies/SongPolicy.php")
	wantReported(t, s.reported(), map[string]bool{
		`App\Policies\SongPolicy`:                   false,
		`App\Models\Billing\Policies\InvoicePolicy`: false,
		`App\Policies\GhostPolicy`:                  true,
		`App\Policies\MixerPolicy`:                  true,
		`App\Support\SongPolicy`:                    true,
		`Other\Policies\SongPolicy`:                 true,
	})
}

// TestFlarumCommandHandlersAreResolvedByTheBus: Flarum's bus runs the class named
// after a dispatched command with a Handler suffix. The handler must take the
// command, the command must be built somewhere, and the code must be built on
// Flarum; a handler whose command nothing builds is still reported.
func TestFlarumCommandHandlersAreResolvedByTheBus(t *testing.T) {
	var s laravelStore
	const ns = `Flarum\User\Command\`
	s.class(ns+"UploadAvatar", "src/User/Command/UploadAvatar.php")
	s.class(ns+"UploadAvatarHandler", "src/User/Command/UploadAvatarHandler.php")
	s.method(ns+"UploadAvatarHandler::handle", "src/User/Command/UploadAvatarHandler.php", names(ns+"UploadAvatar"))
	s.class(ns+"Unbuilt", "src/User/Command/Unbuilt.php")
	s.class(ns+"UnbuiltHandler", "src/User/Command/UnbuiltHandler.php")
	s.method(ns+"UnbuiltHandler::handle", "src/User/Command/UnbuiltHandler.php", names(ns+"Unbuilt"))
	s.class(ns+"Loose", "src/User/Command/Loose.php")
	s.class(ns+"LooseHandler", "src/User/Command/LooseHandler.php")
	s.method(`Flarum\Api\AvatarController::handle`, "src/Api/AvatarController.php",
		facts.Relation{Kind: facts.RelInstantiates, Target: ns + "UploadAvatar"},
		facts.Relation{Kind: facts.RelInstantiates, Target: ns + "Loose"})
	s.class(`Flarum\Api\AvatarController`, "src/Api/AvatarController.php")
	// The same shape outside a Flarum codebase is no bus convention.
	s.class(`Shop\Cmd\Ship`, "shop/Cmd/Ship.php")
	s.class(`Shop\Cmd\ShipHandler`, "shop/Cmd/ShipHandler.php")
	s.method(`Shop\Cmd\ShipHandler::handle`, "shop/Cmd/ShipHandler.php", names(`Shop\Cmd\Ship`))
	s.method(`Shop\Api::run`, "shop/Api.php", facts.Relation{Kind: facts.RelInstantiates, Target: `Shop\Cmd\Ship`})
	wantReported(t, s.reported(), map[string]bool{
		ns + "UploadAvatarHandler": false,
		ns + "UnbuiltHandler":      true,
		ns + "LooseHandler":        true,
	})

	var plain laravelStore
	plain.facts = s.facts[len(s.facts)-4:]
	wantReported(t, plain.reported(), map[string]bool{`Shop\Cmd\ShipHandler`: true})
}
