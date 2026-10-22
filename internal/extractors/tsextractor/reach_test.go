package tsextractor

import (
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// reachApp is a Next.js application whose page renders a component that reaches a
// client through a facade on an object a hook returns, the way no call graph binds:
// `const api = useApi(); api.getItem(id)`. The facade has a method nothing calls,
// the client a method only an interface declares, and a store an action no one
// uses; each holds a request the application never sends.
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
		"src/stores/cart.ts": `
export const useCart = {
  clear: async () => fetch('/api/cart', { method: 'DELETE' }),
};
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
		"DELETE /api/cart":          false, // a store nothing imports
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
