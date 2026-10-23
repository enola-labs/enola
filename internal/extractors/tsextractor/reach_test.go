package tsextractor

import (
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// reachApp is a Next.js application whose page renders a component that reaches a
// client through a facade on an object a hook returns, the way no call graph binds:
// `const api = useApi(); api.getItem(id)`. The facade has a method nothing calls and
// the client a method only an interface declares; each holds a request the
// application never sends.
func reachApp(withReact bool) map[string]string {
	pkg := `{"dependencies": {"next": "14.0.0", "react": "18.2.0"}}`
	if !withReact {
		pkg = `{"dependencies": {"express": "4.18.0"}}`
	}
	return map[string]string{
		"package.json": pkg,
		"src/app/page.tsx": `
import ShopTab from '@/components/ShopTab';
export default function Page() { return <ShopTab />; }
`,
		"src/components/ShopTab.tsx": `
import { useApi } from '@/contexts/api';
export default function ShopTab() {
  const api = useApi();
  const open = async (id: number) => { await api.getItem(id); };
  return <button onClick={() => open(1)}>open</button>;
}
`,
		"src/contexts/api.ts": `
import { apiClient } from '@/lib/api';
export function useApi() { return apiClient; }
`,
		"src/lib/api.ts": `
import { shopClient } from '@/lib/shop';
class ApiClient {
  async getItem(id: number) { return shopClient.getItem(id); }
  async getArchive() { return shopClient.getArchive(); }
}
export const apiClient = new ApiClient();
`,
		"src/lib/shop.ts": `
interface Shop {
  getItem(id: number): Promise<Response>;
  reorder(id: number): Promise<Response>;
}
class ShopClient implements Shop {
  async getItem(id: number) { return fetch(` + "`/api/items/${id}/details`" + `); }
  async getArchive() { return fetch('/api/items/archive'); }
  async reorder(id: number) { return fetch(` + "`/api/items/${id}/reorder`" + `, { method: 'PUT' }); }
}
export const shopClient = new ShopClient();
`,
	}
}

func clientReach(t *testing.T, ff []facts.Fact) map[string]any {
	t.Helper()
	out := map[string]any{}
	for _, f := range ff {
		if f.Kind == facts.KindRoute && f.PropAny(facts.PropRole) == facts.RoleClient {
			v, ok := f.Props[facts.PropClientReachable]
			if !ok {
				v = "absent"
			}
			out[f.PropString("method")+" "+f.Name] = v
		}
	}
	return out
}

func TestClientReachability_ReachedFromThePageThroughAHookAndAFacade(t *testing.T) {
	got := clientReach(t, extractTSRepo(t, reachApp(true)))
	want := map[string]any{
		"GET /api/items/{}/details": true,  // page → ShopTab → api.getItem → shopClient.getItem
		"GET /api/items/archive":    false, // the facade's getArchive: nothing calls it
		"PUT /api/items/{}/reorder": false, // declared by an interface, invoked by no one
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s: client_reachable = %v, want %v (all: %v)", k, got[k], v, got)
		}
	}
}

// A name the reached code uses reaches every symbol that has it: reading by name
// cannot tell two getArchive methods apart, so it calls both reached rather than
// risk calling a live request dead.
func TestClientReachability_ASharedNameReachesBoth(t *testing.T) {
	files := reachApp(true)
	files["src/components/Archive.tsx"] = `
export function Archive({ store }: { store: { getArchive(): void } }) {
  store.getArchive();
  return null;
}
`
	files["src/app/archive/page.tsx"] = `
import { Archive } from '@/components/Archive';
export default function Page() { return <Archive store={{ getArchive() {} }} />; }
`
	got := clientReach(t, extractTSRepo(t, files))
	if got["GET /api/items/archive"] != true {
		t.Errorf("getArchive is used by reached code, so every getArchive is reached: %v", got)
	}
}

// Outside a React application there is no entry point to read from, so no route
// gets a verdict.
func TestClientReachability_NoVerdictWithoutAnApplication(t *testing.T) {
	for k, v := range clientReach(t, extractTSRepo(t, reachApp(false))) {
		if v != "absent" {
			t.Errorf("%s: client_reachable = %v, want no verdict outside a React application", k, v)
		}
	}
}

// In a workspace the application is a package, not the repository: a Next.js app
// under apps/ gets verdicts, and a React library beside it does not, because whoever
// consumes the library makes its calls.
func TestClientReachability_PerApplicationPackage(t *testing.T) {
	files := map[string]string{
		"package.json":          `{"private": true, "workspaces": ["apps/*", "packages/*"]}`,
		"apps/web/package.json": `{"dependencies": {"next": "14.0.0", "react": "18.2.0"}}`,
		"apps/web/src/app/page.tsx": `
import { list } from '../lib/items';
export default function Page() { return <button onClick={() => list()}>go</button>; }
`,
		"apps/web/src/lib/items.ts": `
export async function list() { return fetch('/api/items/list'); }
export async function archive() { return fetch('/api/items/archive'); }
`,
		"packages/ui/package.json": `{"dependencies": {"react": "18.2.0"}}`,
		"packages/ui/src/client.ts": `
export async function loadWidget() { return fetch('/api/widgets/load'); }
`,
	}
	got := clientReach(t, extractTSRepo(t, files))
	want := map[string]any{
		"GET /api/items/list":    true,
		"GET /api/items/archive": false,
		"GET /api/widgets/load":  "absent",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s: client_reachable = %v, want %v (all: %v)", k, got[k], v, got)
		}
	}
}

// Module edges that carry no name a use could match still reach: a component
// default-imported under a name of its own, a page that re-exports its view's
// default, and a component loaded by dynamic(() => import(...)). A file nothing
// imports, a script or a worker, is taken as an entry.
func TestClientReachability_DefaultImportsReexportsAndDynamicImports(t *testing.T) {
	files := map[string]string{
		"package.json": `{"dependencies": {"next": "14.0.0", "react": "18.2.0"}}`,
		"src/app/page.tsx": `
import Modal from '../components/DisableModal';
export default function Page() { return <Modal />; }
`,
		"src/components/DisableModal.tsx": `
export default function DisableTwoFactorModal() {
  return <button onClick={() => fetch('/api/two-factor/disable', { method: 'POST' })}>off</button>;
}
`,
		"src/app/video/page.tsx": `export { default } from '../../views/VideoView';`,
		"src/views/VideoView.tsx": `
function Overlay() { return <button onClick={() => fetch('/api/video/guest-session', { method: 'POST' })}>join</button>; }
export default function VideoView() { return <Overlay />; }
`,
		"src/app/setup/page.tsx": `
import dynamic from 'next/dynamic';
const Setup = dynamic(() => import('../../integrations/Setup'));
export default function Page() { return <Setup />; }
`,
		"src/integrations/Setup.tsx": `
export default function CalendarSetup() {
  return <button onClick={() => fetch('/api/integrations/calendar/add', { method: 'POST' })}>add</button>;
}
`,
		"scripts/cron.ts": `
async function tick() { return fetch('/api/cron/tick'); }
tick();
`,
	}
	got := clientReach(t, extractTSRepo(t, files))
	for _, k := range []string{"POST /api/two-factor/disable", "POST /api/video/guest-session", "POST /api/integrations/calendar/add", "GET /api/cron/tick"} {
		if got[k] != true {
			t.Errorf("%s: client_reachable = %v, want true (all: %v)", k, got[k], got)
		}
	}
}

// A function a component or hook declares by name runs only when that name is used:
// a handler no element is given, or a closure nothing calls, holds a request that is
// never sent. One handed to an effect or a timer runs without its name being read.
func TestClientReachability_LocalFunctionsRunWhenTheirNameIsUsed(t *testing.T) {
	files := map[string]string{
		"package.json": `{"dependencies": {"next": "14.0.0", "react": "18.2.0"}}`,
		"src/app/page.tsx": `
import { useCallback, useEffect } from 'react';
export default function Page() {
  const save = async () => { await fetch('/api/items/save', { method: 'POST' }); };
  const purge = async () => { await fetch('/api/items/purge', { method: 'DELETE' }); };
  const refresh = useCallback(async () => { await fetch('/api/items/refresh'); }, []);
  const rebuild = useCallback(async () => { await fetch('/api/items/rebuild', { method: 'POST' }); }, []);
  function exportAll() { return fetch('/api/items/export'); }
  function importAll() { return fetch('/api/items/import', { method: 'POST' }); }
  useEffect(() => { fetch('/api/items/visit', { method: 'POST' }); }, []);
  setTimeout(async () => { await fetch('/api/items/ping'); }, 1000);
  return <div><button onClick={save}>save</button><button onClick={() => refresh()}>r</button><a onClick={exportAll}>x</a></div>;
}
`,
	}
	got := clientReach(t, extractTSRepo(t, files))
	want := map[string]any{
		"POST /api/items/save":    true,
		"DELETE /api/items/purge": false,
		"GET /api/items/refresh":  true,
		"POST /api/items/rebuild": false,
		"GET /api/items/export":   true,
		"POST /api/items/import":  false,
		"POST /api/items/visit":   true,
		"GET /api/items/ping":     true,
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s: client_reachable = %v, want %v (all: %v)", k, got[k], v, got)
		}
	}
}
