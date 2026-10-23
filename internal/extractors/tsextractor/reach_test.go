package tsextractor

import (
	"fmt"
	"strings"
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

// A hook that returns a closure passes it on; the page that destructures it and never
// calls it never sends the closure's request. Reading the name, destructured or as a
// property of what the hook returned, is what uses it. A shorthand handed to a call,
// `useQuery({ queryFn })`, counts as used: the library calls it by key.
func TestClientReachability_BindingANameIsNotUsingIt(t *testing.T) {
	files := map[string]string{
		"package.json": `{"dependencies": {"next": "14.0.0", "react": "18.2.0"}}`,
		"src/app/page.tsx": `
import { useBridge } from '@/hooks/useBridge';
import { useOrders } from '@/hooks/useOrders';
export default function Page() {
  const { updateStatus, loadItems } = useBridge();
  const orders = useOrders();
  return <div><button onClick={() => loadItems()}>load</button><button onClick={() => orders.cancel(1)}>x</button></div>;
}
`,
		"src/hooks/useBridge.ts": `
import { api } from '@/lib/api';
export const useBridge = () => {
  const updateStatus = async (id: number) => { const t = await api.getTransaction(id); await api.updateTransaction(id, t); };
  const loadItems = async () => api.listItems();
  return { updateStatus, loadItems };
};
`,
		"src/hooks/useOrders.ts": `
import { useMemo } from 'react';
import { useQuery } from '@tanstack/react-query';
export function useOrders() {
  const cancel = async (id: number) => fetch(` + "`/api/orders/${id}/cancel`" + `, { method: 'POST' });
  const archive = async (id: number) => fetch(` + "`/api/orders/${id}/archive`" + `, { method: 'POST' });
  const queryFn = async () => fetch('/api/orders/recent');
  useQuery({ queryKey: ['recent'], queryFn });
  return useMemo(() => ({ cancel, archive }), []);
}
`,
		"src/lib/api.ts": `
class Api {
  async getTransaction(id: number) { return fetch(` + "`/api/transactions/${id}`" + `); }
  async updateTransaction(id: number, t: unknown) { return fetch(` + "`/api/transactions/${id}`" + `, { method: 'PUT' }); }
  async listItems() { return fetch('/api/items'); }
}
export const api = new Api();
`,
	}
	got := clientReach(t, extractTSRepo(t, files))
	want := map[string]any{
		"GET /api/transactions/{}":    false,
		"PUT /api/transactions/{}":    false,
		"GET /api/items":              true,
		"POST /api/orders/{}/cancel":  true,
		"POST /api/orders/{}/archive": false,
		"GET /api/orders/recent":      true,
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s: client_reachable = %v, want %v (all: %v)", k, got[k], v, got)
		}
	}
}

// clientTriggers is each client route's trigger props: "trigger every_page poll_ms".
func clientTriggers(t *testing.T, ff []facts.Fact) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, f := range ff {
		if f.Kind == facts.KindRoute && f.PropAny(facts.PropRole) == facts.RoleClient {
			v := f.PropString(facts.PropClientTrigger)
			if f.PropAny(facts.PropClientEveryPage) == true {
				v += " every-page"
			}
			if ms, ok := f.Props[facts.PropClientPollMs]; ok {
				v += fmt.Sprintf(" %vms", ms)
			}
			out[f.PropString("method")+" "+f.Name] = v
		}
	}
	return out
}

// How each call is triggered, read from the code that runs it: a timer in a provider
// the root layout renders polls on every page; a constant interval is stated; a
// react-query fetcher with a refetch interval polls; a page's mount effect loads; an
// event handler, inline or named, acts; a function both mounted and polled is polled.
func TestClientTriggers(t *testing.T) {
	files := map[string]string{
		"package.json": `{"dependencies": {"next": "14.0.0", "react": "18.2.0"}}`,
		"src/app/layout.tsx": `
import { NotificationProvider } from '@/contexts/notifications';
export default function RootLayout({ children }) { return <NotificationProvider>{children}</NotificationProvider>; }
`,
		"src/contexts/notifications.tsx": `
import { useEffect, useCallback } from 'react';
const UNREAD_POLL = 60 * 1000;
export function NotificationProvider({ children }) {
  const fetchUnread = useCallback(async () => { await fetch('/api/messages/unread-count'); }, []);
  useEffect(() => {
    const id = setInterval(() => { fetch('/api/notifications'); }, 60000);
    const id2 = setInterval(fetchUnread, UNREAD_POLL);
    return () => { clearInterval(id); clearInterval(id2); };
  }, []);
  return children;
}
`,
		"src/stores/feed.ts": `
export async function loadFeed() { return fetch('/api/feed'); }
export async function loadStats() { return fetch('/api/stats'); }
`,
		"src/app/feed/page.tsx": `
import { useEffect } from 'react';
import { useQuery } from '@tanstack/react-query';
import { loadFeed, loadStats } from '@/stores/feed';
export default function Page() {
  useEffect(() => { loadFeed(); }, []);
  useQuery({ queryKey: ['stats'], queryFn: loadStats, refetchInterval: 30_000 });
  const save = async () => { await fetch('/api/feed/save', { method: 'POST' }); };
  return <div>
    <button onClick={() => fetch('/api/feed/export', { method: 'POST' })}>export</button>
    <button onClick={save}>save</button>
  </div>;
}
`,
		"src/app/stats/page.tsx": `
import { useEffect } from 'react';
import { loadStats } from '@/stores/feed';
export default function Page() { useEffect(() => { loadStats(); }, []); return null; }
`,
	}
	// A store's wait-loop polls zustand's get, a parameter: it names no function of
	// the module, though an API client declares a get of its own.
	files["src/stores/cache.ts"] = `
import { create } from 'zustand';
export const useCache = create((set, get) => ({
  waitForTypes: () => new Promise((resolve) => {
    const id = setInterval(() => { if (get().types) { clearInterval(id); resolve(get().types); } }, 100);
  }),
}));
`
	files["src/lib/http.ts"] = `
export class Http {
  async get(path: string) { return fetch('/api/everything'); }
}
`
	files["src/app/cache/page.tsx"] = `
import { useEffect } from 'react';
import { useCache } from '@/stores/cache';
import { Http } from '@/lib/http';
export default function Page() { useEffect(() => { useCache.getState().waitForTypes(); new Http().get('/x'); }, []); return null; }
`
	// The provider polls its own fetchNotifications; another file's store method of the
	// same name is not polled by it.
	files["src/contexts/notifications.tsx"] = strings.Replace(files["src/contexts/notifications.tsx"],
		"const fetchUnread =", "const fetchNotifications = async () => { await fetch('/api/notifications/mine'); };\n  const fetchUnread =", 1)
	files["src/contexts/notifications.tsx"] = strings.Replace(files["src/contexts/notifications.tsx"],
		"const id2 =", "const id3 = setInterval(() => { fetchNotifications(); }, 60000);\n    const id2 =", 1)
	files["src/stores/reception.ts"] = `
export const reception = {
  async fetchNotifications() { return fetch('/api/reception/orders'); },
};
`
	files["src/app/reception/page.tsx"] = `
import { reception } from '@/stores/reception';
export default function Page() { return <button onClick={() => reception.fetchNotifications()}>load</button>; }
`
	got := clientTriggers(t, extractTSRepo(t, files))
	want := map[string]string{
		"GET /api/everything":            "mount",
		"GET /api/notifications/mine":    "poll every-page 60000ms",
		"GET /api/reception/orders":      "action",
		"GET /api/notifications":         "poll every-page 60000ms",
		"GET /api/messages/unread-count": "poll every-page 60000ms",
		"GET /api/feed":                  "mount",
		"GET /api/stats":                 "poll 30000ms",
		"POST /api/feed/export":          "action",
		"POST /api/feed/save":            "action",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s: trigger %q, want %q (all: %v)", k, got[k], v, got)
		}
	}
}
