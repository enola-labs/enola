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
