package phpextractor

import (
	"context"
	"sort"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// routeKeys returns "METHOD path -> handled_by" for every route fact, sorted.
func routeKeys(ff []facts.Fact) []string {
	var out []string
	for _, f := range ff {
		if f.Kind != facts.KindRoute {
			continue
		}
		key := f.PropString("method") + " " + f.Name + " ->"
		for _, r := range f.Relations {
			if r.Kind == facts.RelHandledBy {
				key += " " + r.Target
			}
		}
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

// infoXMLNames returns the classes the file reference of rel names, sorted.
func infoXMLNames(ff []facts.Fact, rel string) []string {
	var out []string
	for _, f := range ff {
		if f.Kind != facts.KindFileRef || f.File != rel {
			continue
		}
		for _, r := range f.Relations {
			if r.Kind == facts.RelNames {
				out = append(out, r.Target)
			}
		}
	}
	sort.Strings(out)
	return out
}

const ncInfoXML = `<?xml version="1.0"?>
<info>
	<id>contacts</id>
	<name>Contacts</name>
	<description>Uses OCA\Contacts\Service\Mentioned in prose.</description>
	<background-jobs>
		<job>OCA\Contacts\Cron\SocialUpdate</job>
	</background-jobs>
	<settings>
		<admin>OCA\Contacts\Settings\AdminSettings</admin>
	</settings>
	<contactsmenu>
		<provider>OCA\Contacts\ContactsMenu\DetailsProvider</provider>
	</contactsmenu>
	<commands>
		<command>OCA\Vendor\NotDeclared</command>
	</commands>
</info>
`

const ncRoutes = `<?php
return [
	'routes' => [
		['name' => 'contacts#direct', 'url' => '/direct/contact/{contact}', 'verb' => 'GET'],
		['name' => 'contacts#directcircle', 'url' => '/direct/circle/{singleId}', 'verb' => 'GET'],
		['name' => 'page#index', 'url' => '/'],
		['name' => 'social_api#update_contact', 'url' => '/api/v1/social/avatar/{network}', 'verb' => 'PUT'],
		['name' => 'page#missing', 'url' => '/missing', 'verb' => 'GET'],
	],
	'ocs' => [
		['name' => 'social_api#get_user_config', 'url' => '/api/v1/config/{key}', 'verb' => 'GET'],
	],
	'resources' => [
		'note' => ['url' => '/notes'],
	],
];
`

// TestNextcloudRoutesAreServedByTheirController: an app's appinfo/routes.php names
// each action `controller#action`, which Nextcloud resolves to
// OCA\<App>\Controller\<Controller>Controller::<action> (snake case to camel case,
// matched without regard to case as PHP does). A web route is served under
// /apps/<id>, an OCS route under /ocs/v2.php/apps/<id>, and a resource expands to
// its five actions. An action the controller does not declare has no handler.
func TestNextcloudRoutesAreServedByTheirController(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "appinfo/info.xml", ncInfoXML)
	writeFile(t, dir, "appinfo/routes.php", ncRoutes)
	writeFile(t, dir, "lib/Controller/ContactsController.php", `<?php
namespace OCA\Contacts\Controller;
class ContactsController {
	public function direct(string $contact) {}
	public function directCircle(string $singleId) {}
}
`)
	writeFile(t, dir, "lib/Controller/PageController.php", `<?php
namespace OCA\Contacts\Controller;
class PageController { public function index() {} }
`)
	writeFile(t, dir, "lib/Controller/SocialApiController.php", `<?php
namespace OCA\Contacts\Controller;
class SocialApiController { public function updateContact() {} public function getUserConfig() {} }
`)
	writeFile(t, dir, "lib/Controller/NoteController.php", `<?php
namespace OCA\Contacts\Controller;
class NoteController { public function index() {} public function show($id) {} public function create() {} public function update($id) {} public function destroy($id) {} }
`)
	files := []string{"appinfo/info.xml", "appinfo/routes.php", "lib/Controller/ContactsController.php",
		"lib/Controller/PageController.php", "lib/Controller/SocialApiController.php", "lib/Controller/NoteController.php"}
	ff, err := (&PHPExtractor{}).Extract(context.Background(), dir, files)
	if err != nil {
		t.Fatal(err)
	}
	const c = `OCA\Contacts\Controller\`
	wantTargets(t, "routes", routeKeys(ff),
		"GET /apps/contacts/direct/contact/{contact} -> "+c+"ContactsController::direct",
		"GET /apps/contacts/direct/circle/{singleId} -> "+c+"ContactsController::directCircle",
		"GET /apps/contacts -> "+c+"PageController::index",
		"PUT /apps/contacts/api/v1/social/avatar/{network} -> "+c+"SocialApiController::updateContact",
		"GET /apps/contacts/missing ->",
		"GET /ocs/v2.php/apps/contacts/api/v1/config/{key} -> "+c+"SocialApiController::getUserConfig",
		"GET /apps/contacts/notes -> "+c+"NoteController::index",
		"GET /apps/contacts/notes/{id} -> "+c+"NoteController::show",
		"POST /apps/contacts/notes -> "+c+"NoteController::create",
		"PUT /apps/contacts/notes/{id} -> "+c+"NoteController::update",
		"DELETE /apps/contacts/notes/{id} -> "+c+"NoteController::destroy",
	)
}

// TestNextcloudRouteAttributesAreRoutes: #[ApiRoute] on a controller action is an
// OCS route of the app the file belongs to, #[FrontpageRoute] a web route, with the
// verb and url positional or named. An attribute of that short name from another
// namespace is no route.
func TestNextcloudRouteAttributesAreRoutes(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "apps/notes/appinfo/info.xml", "<info><id>notes</id><namespace>Notes</namespace></info>")
	writeFile(t, dir, "apps/notes/lib/Controller/ImportController.php", `<?php
namespace OCA\Notes\Controller;

use OCP\AppFramework\Http\Attribute\ApiRoute;
use OCP\AppFramework\Http\Attribute\FrontpageRoute;
use OCP\AppFramework\Http\Attribute\NoAdminRequired;

class ImportController {
	#[NoAdminRequired]
	#[ApiRoute('POST', '/api/v1/import/{fileId}')]
	public function import(int $fileId) {}

	#[FrontpageRoute(url: '/export', verb: 'GET')]
	public function export() {}

	#[\Other\ApiRoute('GET', '/nope')]
	public function other() {}
}
`)
	files := []string{"apps/notes/appinfo/info.xml", "apps/notes/lib/Controller/ImportController.php"}
	ff, err := (&PHPExtractor{}).Extract(context.Background(), dir, files)
	if err != nil {
		t.Fatal(err)
	}
	const c = `OCA\Notes\Controller\ImportController::`
	wantTargets(t, "routes", routeKeys(ff),
		"POST /ocs/v2.php/apps/notes/api/v1/import/{fileId} -> "+c+"import",
		"GET /apps/notes/export -> "+c+"export",
	)
	for _, f := range ff {
		if _, ok := f.Props[propNextcloudRoutes]; ok {
			t.Fatalf("%s keeps %s", f.Name, propNextcloudRoutes)
		}
	}
}

// TestNextcloudInfoXMLNamesTheClassesItRegisters: appinfo/info.xml registers
// background jobs, settings, providers and commands by class name. Each element
// whose whole text is a declared class is a use; a class written in prose, or one
// the repository does not declare, is not.
func TestNextcloudInfoXMLNamesTheClassesItRegisters(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "appinfo/info.xml", ncInfoXML)
	files := []string{"appinfo/info.xml"}
	for rel, class := range map[string]string{
		"lib/Cron/SocialUpdate.php":            "namespace OCA\\Contacts\\Cron;\nclass SocialUpdate {}",
		"lib/Settings/AdminSettings.php":       "namespace OCA\\Contacts\\Settings;\nclass AdminSettings {}",
		"lib/ContactsMenu/DetailsProvider.php": "namespace OCA\\Contacts\\ContactsMenu;\nclass DetailsProvider {}",
		"lib/Service/Mentioned.php":            "namespace OCA\\Contacts\\Service;\nclass Mentioned {}",
	} {
		writeFile(t, dir, rel, "<?php\n"+class+"\n")
		files = append(files, rel)
	}
	ff, err := (&PHPExtractor{}).Extract(context.Background(), dir, files)
	if err != nil {
		t.Fatal(err)
	}
	wantTargets(t, "info.xml names", infoXMLNames(ff, "appinfo/info.xml"),
		`OCA\Contacts\ContactsMenu\DetailsProvider`, `OCA\Contacts\Cron\SocialUpdate`, `OCA\Contacts\Settings\AdminSettings`)
}
