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

// namesInTheirModulePages is two features and three pages. The reports page uses words
// another feature declares — a prop it destructures (renderItem), a constant of its own
// (COLUMNS), an enum member (Draft), a type (Row), a member (options.locale) — and
// reaches none of that feature's code. The tags page reaches its call through a
// barrel that renames the component it re-exports, and a namespace import.
func namesInTheirModulePages() map[string]string {
	return map[string]string{
		"package.json": `{"dependencies": {"next": "14.0.0", "react": "18.2.0"}}`,
		"src/app/tags/page.tsx": `
import { TagsPage } from '@/features/tags/server';
export default TagsPage;
`,
		"src/features/tags/server.ts": `
export { Page as TagsPage } from './app/tags';
`,
		"src/features/tags/app/tags.tsx": `
import * as api from '../data/api';
export const Page = async () => {
  const tags = await api.getTags();
  return <div>{tags}</div>;
};
`,
		"src/features/tags/data/api.ts": `
export const getTags = async () => fetch('/api/tags');
`,
		"src/app/reports/page.tsx": `
import List from '@/shared/list';
enum Step { Draft = 'draft' }
const COLUMNS = ['a'];
type Row = { title: string };
export default function ReportsPage({ options }: { options: { locale: string } }) {
  const row: Row = { title: options.locale };
  return <List renderItem={(x: string) => x} columns={COLUMNS} step={Step.Draft} row={row} />;
}
`,
		"src/shared/list.tsx": `
export default function List({ renderItem }: { renderItem: (x: string) => string }) {
  return <div>{renderItem('a')}</div>;
}
`,
		"src/app/pay/page.tsx": `
import { renderItem } from '@/features/pay/table';
export default function PayPage() {
  return <div>{renderItem()}</div>;
}
`,
		"src/features/pay/table.tsx": `
import { fetchCards } from './api';
export const COLUMNS = ['x'];
export function renderItem() { return fetchCards(); }
export function Draft() { return fetchCards(); }
export function Row() { return fetchCards(); }
const locale = fetchCards;
export function useLocale() { return locale(); }
`,
		"src/features/pay/api.ts": `
export const fetchCards = () => fetch('/api/cards');
`,
	}
}

// A name resolves in its own module first: the reports page reaches none of the pay
// feature's code through words they share, and the tags page reaches its call
// through a renamed re-export.
func TestClientPages_NamesResolveInTheirOwnModule(t *testing.T) {
	pages := map[string][]string{}
	for _, f := range extractTSRepo(t, namesInTheirModulePages()) {
		if f.Kind != facts.KindRoute || f.PropAny(facts.PropRole) != facts.RoleClient {
			continue
		}
		got, _ := f.PropAny(facts.PropClientPages).([]string)
		pages[f.Name] = got
	}
	if got := pages["/api/cards"]; !slices.Equal(got, []string{"src/app/pay/page.tsx"}) {
		t.Errorf("/api/cards pages = %v, want only the pay page", got)
	}
	if got := pages["/api/tags"]; !slices.Equal(got, []string{"src/app/tags/page.tsx"}) {
		t.Errorf("/api/tags pages = %v, want the tags page", got)
	}
}

// barrelShapesPages is a feature whose pages re-export their component from the
// feature's barrel. Both components are called Page, in two files of one directory, and
// each page must reach its own: the list page reads through a renamed import, the
// edit page through an options factory whose mutationFn a library calls.
func barrelShapesPages() map[string]string {
	return map[string]string{
		"package.json": `{"dependencies": {"next": "14.0.0", "react": "18.2.0", "@tanstack/react-query": "5.0.0"}}`,
		"src/app/notes/page.tsx": `export { NotesPage as default } from '@/features/notes/server';
`,
		"src/app/notes/edit/page.tsx": `export { EditNotePage as default } from '@/features/notes/server';
`,
		"src/features/notes/server.ts": `
export { Page as NotesPage } from './app/notes';
export { Page as EditNotePage } from './app/edit-note';
`,
		"src/features/notes/app/notes.tsx": `
import { listNotes as listNotesApi } from '../data';
export const Page = async () => {
  const notes = await listNotesApi();
  return <div>{notes}</div>;
};
`,
		"src/features/notes/app/edit-note.tsx": `
import { useMutation } from '@tanstack/react-query';
import { saveNoteMutation } from '../data';
export const Page = () => {
  const save = useMutation(saveNoteMutation());
  return <button onClick={() => save.mutate()}>save</button>;
};
`,
		"src/features/notes/data/index.ts": `
export { listNotes, saveNoteMutation } from './api';
`,
		"src/features/notes/data/api.ts": `
export const listNotes = async () => fetch('/api/notes');
export const saveNoteMutation = () => ({
  mutationFn: async () => fetch('/api/notes', { method: 'POST' }),
});
`,
	}
}

// Each page reaches its own component through the barrel, though both are named Page.
func TestClientPages_ThroughABarrelEachPageReachesItsOwn(t *testing.T) {
	pages := map[string][]string{}
	for _, f := range extractTSRepo(t, barrelShapesPages()) {
		if f.Kind != facts.KindRoute || f.PropAny(facts.PropRole) != facts.RoleClient {
			continue
		}
		got, _ := f.PropAny(facts.PropClientPages).([]string)
		pages[f.PropString("method")+" "+f.Name] = got
	}
	if got := pages["GET /api/notes"]; !slices.Equal(got, []string{"src/app/notes/page.tsx"}) {
		t.Errorf("GET /api/notes pages = %v, want the notes page", got)
	}
	if got := pages["POST /api/notes"]; !slices.Equal(got, []string{"src/app/notes/edit/page.tsx"}) {
		t.Errorf("POST /api/notes pages = %v, want the edit page", got)
	}
}

// A call reached from every page of an app with more than everyPageListMax pages is
// client_every_page, with no list.
func TestClientPages_EveryPageOfALargeAppIsEveryPage(t *testing.T) {
	files := map[string]string{
		"package.json": `{"dependencies": {"next": "14.0.0", "react": "18.2.0"}}`,
		"src/lib/session.ts": `export const loadSession = async () => fetch('/api/session');
`,
	}
	for i := 0; i <= everyPageListMax; i++ {
		files["src/app/p"+string(rune('a'+i))+"/page.tsx"] = `
import { loadSession } from '@/lib/session';
export default async function P() { await loadSession(); return <div />; }
`
	}
	for _, f := range extractTSRepo(t, files) {
		if f.Kind != facts.KindRoute || f.Name != "/api/session" {
			continue
		}
		if !f.PropBool(facts.PropClientEveryPage) {
			t.Errorf("client_every_page not set")
		}
		if got, _ := f.PropAny(facts.PropClientPages).([]string); len(got) != 0 {
			t.Errorf("client_pages = %d pages, want none listed", len(got))
		}
		return
	}
	t.Fatal("no /api/session call")
}
