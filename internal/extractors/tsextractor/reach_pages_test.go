package tsextractor

import (
	"slices"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// storePages is two pages over one zustand store whose actions a selector hook hands
// out. The orders page takes fetchOrders and renders a form that takes createOrder; the
// account page takes fetchOrders alone. deleteOrder is in the selector and nobody
// takes it.
func storePages() map[string]string {
	return map[string]string{
		"package.json": `{"dependencies": {"next": "14.0.0", "react": "18.2.0", "zustand": "4.5.0"}}`,
		"src/app/orders/page.tsx": `
import { useOrderActions } from '@/stores/orders';
import OrderForm from '@/components/OrderForm';
export default function OrdersPage() {
  const { fetchOrders } = useOrderActions();
  fetchOrders();
  return <OrderForm />;
}
`,
		"src/app/account/page.tsx": `
import { useOrderActions } from '@/stores/orders';
export default function AccountPage() {
  const { fetchOrders } = useOrderActions();
  fetchOrders();
  return <div />;
}
`,
		"src/components/OrderForm.tsx": `
import { useOrderActions } from '@/stores/orders';
export default function OrderForm() {
  const { createOrder } = useOrderActions();
  return <button onClick={() => createOrder()}>save</button>;
}
`,
		"src/stores/orders.ts": `
import { create } from 'zustand';
import { useShallow } from 'zustand/react/shallow';
import { ordersApi } from '@/lib/orders';
export const useOrderStore = create((set) => ({
  fetchOrders: async () => { await ordersApi.list(); },
  createOrder: async () => { await ordersApi.create(); },
  deleteOrder: async (id: number) => { await ordersApi.remove(id); },
}));
export const useOrderActions = () =>
  useOrderStore(
    useShallow((state) => ({
      fetchOrders: state.fetchOrders,
      createOrder: state.createOrder,
      deleteOrder: state.deleteOrder,
    }))
  );
`,
		"src/lib/orders.ts": `
class OrdersApi {
  async list() { return fetch('/api/orders'); }
  async create() { return fetch('/api/orders', { method: 'POST' }); }
  async remove(id: number) { return fetch(` + "`/api/orders/${id}`" + `, { method: 'DELETE' }); }
}
export const ordersApi = new OrdersApi();
`,
	}
}

// Each reached client call names the pages whose code reaches it, and a selector
// copying an action under its own name hands it on without using it: deleteOrder,
// which no component takes, is never sent and names no page.
func TestClientPages_ThePagesThatReachACall(t *testing.T) {
	pages := map[string][]string{}
	reached := map[string]any{}
	for _, f := range extractTSRepo(t, storePages()) {
		if f.Kind != facts.KindRoute || f.PropAny(facts.PropRole) != facts.RoleClient {
			continue
		}
		key := f.PropString("method") + " " + f.Name
		reached[key] = f.PropAny(facts.PropClientReachable)
		if p, ok := f.PropAny(facts.PropClientPages).([]string); ok {
			pages[key] = p
		}
	}
	want := map[string][]string{
		"GET /api/orders":  {"src/app/account/page.tsx", "src/app/orders/page.tsx"},
		"POST /api/orders": {"src/app/orders/page.tsx"},
	}
	for k, v := range want {
		if !slices.Equal(pages[k], v) {
			t.Errorf("%s: client_pages = %v, want %v (all: %v)", k, pages[k], v, pages)
		}
	}
	if reached["DELETE /api/orders/{}"] != false || pages["DELETE /api/orders/{}"] != nil {
		t.Errorf("DELETE /api/orders/{}: reachable %v, pages %v; want never sent, no page", reached["DELETE /api/orders/{}"], pages["DELETE /api/orders/{}"])
	}
}
