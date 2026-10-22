package tsextractor

import (
	"slices"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// A shared client another file exports, the shape a whole frontend made its requests
// through while none of them was a route: a class method hands its first argument to
// fetch after the base URL, and the file exports an instance. Each call through the
// imported instance is a request to the path it passes, with the method its options
// name. A class of another file that forwards its own URL to the shared client is a
// helper too, so `this.makeRequest(url)` resolves, through a local built by a
// conditional whose branches differ only in the query string. The shared client's own
// fetch is no request to "/": it goes wherever its callers send it.
func TestImportedWrapper_CallsThroughAnExportedClientAreRoutes(t *testing.T) {
	ff := extractTSRepo(t, map[string]string{
		"src/lib/api/auth.ts": `
const API_BASE_URL = process.env.API;

export class AuthApiClient {
  async handleAuthenticatedRequest(url: string, options: RequestInit = {}): Promise<Response> {
    const fullUrl = ` + "`${API_BASE_URL}${url}`" + `;
    return fetch(fullUrl, options);
  }
}

export const authApiClient = new AuthApiClient();
`,
		"src/lib/api/proshops.ts": `
import { authApiClient } from './auth';

export async function listProShops() {
  return authApiClient.handleAuthenticatedRequest('/api/settings/proshops', { method: 'GET' });
}

export async function removeProShop(id: number) {
  return authApiClient.handleAuthenticatedRequest(` + "`/api/settings/proshops/${id}`" + `, { method: 'DELETE' });
}
`,
		"src/lib/api/marketplace.ts": `
import { authApiClient } from './auth';

export class MarketplaceApiClient {
  private async makeRequest(endpoint: string, options: RequestInit = {}) {
    return authApiClient.handleAuthenticatedRequest(` + "`/api${endpoint}`" + `, options);
  }

  async listAds() {
    return this.makeRequest('/settings/marketplace/ads');
  }
}
`,
		"src/lib/api/shop.ts": `
import { authApiClient as client } from './auth';

export class ShopApiClient {
  private async makeRequest<T>(url: string, options: RequestInit = {}): Promise<T> {
    const response = await client.handleAuthenticatedRequest(url, options);
    return response.json();
  }

  async getAllInventory(shopId?: number | null): Promise<unknown[]> {
    try {
      const url = shopId
        ? ` + "`/api/settings/inventory?pro_shop_id=${shopId}`" + `
        : '/api/settings/inventory';
      const result = await this.makeRequest<unknown[]>(url);
      return Array.isArray(result) ? result : [];
    } catch (error) {
      return [];
    }
  }
}
`,
	})
	var got []string
	for _, f := range ff {
		if f.Kind == facts.KindRoute && f.PropString(facts.PropRole) == facts.RoleClient {
			got = append(got, f.PropString("method")+" "+f.Name+" @"+f.File)
		}
	}
	for _, want := range []string{
		"GET /api/settings/proshops @src/lib/api/proshops.ts",
		"DELETE /api/settings/proshops/{} @src/lib/api/proshops.ts",
		"GET /api/settings/inventory @src/lib/api/shop.ts",
		"GET /api/settings/marketplace/ads @src/lib/api/marketplace.ts",
	} {
		if !slices.Contains(got, want) {
			t.Errorf("client routes = %v, want %q", got, want)
		}
	}
	for _, g := range got {
		if g == "GET / @src/lib/api/auth.ts" || g == "GET /api @src/lib/api/marketplace.ts" {
			t.Errorf("a helper's own request, to its caller's URL, was read as a route: %v", got)
		}
	}
}

// A helper can make its request through a helper method of an instance its own file
// declares: `deduplicatedFetch(url)` hands url to `requestCache.fetch(url)`, which
// hands it to `this.makeRequest(url)`, which calls fetch. A call to it that puts an unresolved base before the path,
// `${API_BASE_URL}/api/...`, is a request to the path, the way the helper's own
// `${API_BASE_URL}${url}` is read.
func TestImportedWrapper_AHelperThroughASameFileInstance(t *testing.T) {
	ff := extractTSRepo(t, map[string]string{
		"src/lib/requestCache.ts": `
class RequestCache {
  async fetch<T>(url: string, options: RequestInit = {}): Promise<T> {
    const promise = this.makeRequest<T>(url, options);
    return promise;
  }
  private async makeRequest<T>(url: string, options?: RequestInit): Promise<T> {
    const response = await fetch(url, options);
    return response.json();
  }
}
export const requestCache = new RequestCache();

export function deduplicatedFetch<T>(url: string, options?: RequestInit): Promise<T> {
  return requestCache.fetch<T>(url, options);
}
`,
		"src/stores/inventory.ts": `
import { deduplicatedFetch } from '../lib/requestCache';
const API_BASE_URL = process.env.API;
export async function loadStatuses() {
  return deduplicatedFetch<string[]>(` + "`${API_BASE_URL}/api/settings/transactions/statuses`" + `, { method: 'GET' });
}
`,
	})
	found := false
	for _, f := range ff {
		if f.Kind == facts.KindRoute && f.PropString(facts.PropRole) == facts.RoleClient && f.File == "src/stores/inventory.ts" &&
			f.Name == "/api/settings/transactions/statuses" && f.PropString("method") == "GET" {
			found = true
		}
	}
	if !found {
		t.Errorf("the call through deduplicatedFetch should be a client route to GET /api/settings/transactions/statuses")
	}
}

// A method whose parameter fills a segment after a slash, or a query value, takes an id
// or a filter, not a path: it is an endpoint function, and its own request stays a
// route rather than being read as a helper's request to its caller's URL.
func TestImportedWrapper_AnIdTakingMethodIsNoHelper(t *testing.T) {
	ff := extractTSRepo(t, map[string]string{
		"src/lib/items.ts": `
class ItemsClient {
  async getItem(id: number) { return fetch(` + "`/api/items/${id}`" + `); }
  async getRange(date: string) { return fetch(` + "`/api/weather/range?date=${date}`" + `); }
}
export const itemsClient = new ItemsClient();
`,
	})
	got := map[string]bool{}
	for _, f := range ff {
		if f.Kind == facts.KindRoute && f.PropString(facts.PropRole) == facts.RoleClient && f.File == "src/lib/items.ts" {
			got[f.Name] = true
		}
	}
	for _, want := range []string{"/api/items/{}", "/api/weather/range"} {
		if !got[want] {
			t.Errorf("the request to %s was dropped as a helper's own request: %v", want, got)
		}
	}
}

// A helper that sets the method on its own request sends that method, whatever the
// call passes or leaves out: `fetchPost(url, body)` is a POST. A helper that passes
// the caller's options through takes the method from the call.
func TestImportedWrapper_AHelperSendsItsOwnMethod(t *testing.T) {
	ff := extractTSRepo(t, map[string]string{
		"src/data/fetchers.ts": `
export async function fetchPost<T>(url: string, data: unknown): Promise<T> {
  const response = await fetch(url, { method: 'POST', body: JSON.stringify(data) });
  return response.json();
}
export async function fetchAny<T>(url: string, options?: RequestInit): Promise<T> {
  const response = await fetch(url, options);
  return response.json();
}
`,
		"src/data/policy.ts": `
import { fetchPost, fetchAny } from './fetchers';
const BASE_PATH = process.env.BASE;
export async function suggestPolicy(sql: string) {
  return fetchPost(` + "`${BASE_PATH}/api/ai/sql/policy`" + `, { sql });
}
export async function removePolicy(id: string) {
  return fetchAny(` + "`/api/policies/${id}`" + `, { method: 'DELETE' });
}
`,
	})
	got := map[string]bool{}
	for _, f := range ff {
		if f.Kind == facts.KindRoute && f.PropString(facts.PropRole) == facts.RoleClient && f.File == "src/data/policy.ts" {
			got[f.PropString("method")+" "+f.Name] = true
		}
	}
	for _, want := range []string{"POST /api/ai/sql/policy", "DELETE /api/policies/{}"} {
		if !got[want] {
			t.Errorf("want %s among %v", want, got)
		}
	}
	if got["GET /api/ai/sql/policy"] {
		t.Errorf("a call through fetchPost was read as a GET: %v", got)
	}
}
