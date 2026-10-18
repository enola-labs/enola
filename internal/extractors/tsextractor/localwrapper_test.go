package tsextractor

import (
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// The shape a whole file of requests was made through, and none of them was a route:
// a helper declared at the top of the file puts its first argument at the end of the
// URL it hands fetch, after a literal prefix, and every call in the file goes through
// it. The URL local is declared twice (a deduplicated GET branch and the rest), both
// times the same, which the single-assignment fold alone refuses.
func TestLocalWrapper_CallsBecomeClientRoutesWithThePrefix(t *testing.T) {
	src := `
import { getApiBaseUrl } from '@/lib/apiConfig';

const API_BASE_URL = getApiBaseUrl();

const apiRequest = async (url: string, options: RequestInit = {}): Promise<any> => {
  const method = options.method || 'GET';
  if (method === 'GET') {
    return withDeduplication(` + "`tripEvents-${url}`" + `, async () => {
      const fullUrl = ` + "`${API_BASE_URL}/api${url}`" + `;
      const response = await fetch(fullUrl, { ...options, credentials: 'include' });
      return response.json();
    });
  }
  const fullUrl = ` + "`${API_BASE_URL}/api${url}`" + `;
  const response = await fetch(fullUrl, { ...options, credentials: 'include' });
  return response.json();
};

export const tripEventsApi = {
  async getParticipants(eventId: number) {
    return await apiRequest(` + "`/settings/trip-events/${eventId}/participants`" + `);
  },
  async create(data: unknown) {
    return await apiRequest('/settings/trip-events', {
      method: 'POST',
      body: JSON.stringify(data),
    });
  },
  async register(id: number) {
    return apiRequest(` + "`/settings/trip-events/${id}/register`" + `, { method: 'POST' });
  },
};
`
	ff := extractTS(t, src, "src/lib/api/tripEvents.ts")
	got := clientRoutes(ff)
	for path, want := range map[string]string{
		"/api/settings/trip-events/{}/participants": "GET",
		"/api/settings/trip-events":                 "POST",
		"/api/settings/trip-events/{}/register":     "POST",
	} {
		if got[path] != want {
			t.Errorf("%s: want %s, got %+v", path, want, got)
		}
	}
	// The helper's own fetch is not a request to anywhere: its URL is the argument.
	for path := range got {
		if path == "/api" || path == "/api/{}" || path == "/{}" {
			t.Errorf("the wrapper's own fetch was emitted as a route %q: %+v", path, got)
		}
	}
	for _, f := range ff {
		if f.Kind == facts.KindRoute && f.Name == "/api/settings/trip-events" {
			if f.PropString("client_wrapper") != "apiRequest" {
				t.Errorf("a route read through a wrapper must name it: %+v", f.Props)
			}
		}
	}
}

// No prefix at all: the helper passes its argument to fetch unchanged.
func TestLocalWrapper_NoPrefix(t *testing.T) {
	src := `
async function request(path: string, init?: RequestInit) {
  const res = await fetch(path, init);
  return res.json();
}

export function listOrders() {
  return request('/v1/orders');
}
`
	if got := clientRoutes(extractTS(t, src, "src/orders.ts")); got["/v1/orders"] != "GET" {
		t.Errorf("want GET /v1/orders, got %+v", got)
	}
}

// What is not a wrapper stays silent. Each would otherwise invent a path: the argument
// is not at the end, the URL local is reassigned, its declarations disagree, or the
// wrapper is called from a file that only imports it (a cross-file wrapper is not read).
func TestLocalWrapper_NotAWrapper(t *testing.T) {
	cases := map[string]string{
		"argument not last": `
const call = async (id: string) => fetch(` + "`${BASE}/api/${id}/details`" + `);
export const a = () => call('/orders');
`,
		"local reassigned": `
const call = async (url: string) => {
  let full = ` + "`${BASE}/api${url}`" + `;
  full = full + '?debug=1';
  return fetch(full);
};
export const a = () => call('/orders');
`,
		"declarations disagree": `
const call = async (url: string, admin: boolean) => {
  if (admin) {
    const full = ` + "`${BASE}/admin${url}`" + `;
    return fetch(full);
  }
  const full = ` + "`${BASE}/api${url}`" + `;
  return fetch(full);
};
export const a = () => call('/orders', false);
`,
		// The base is not unknown: the file binds it to a third party's absolute URL.
		// Dropping it as "the base" turned a GitHub API call into a relative path.
		"base is a third-party URL": `
const ISSUE_API = ` + "`https://api.github.com/repos/${REPO}/issues`" + `;
async function withGithub(path, options = {}) {
  return fetch(` + "`${ISSUE_API}${path}`" + `, options);
}
export const a = () => withGithub('/orders/comments');
`,
		"imported, not declared here": `
import { apiRequest } from './client';
export const a = () => apiRequest('/settings/orders');
`,
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			// Only the CALL is under test. A helper's own fetch with a literal URL
			// (the "argument not last" case) is an ordinary request and stays one.
			for path := range clientRoutes(extractTS(t, src, "src/x.ts")) {
				if strings.Contains(path, "orders") {
					t.Errorf("the call was read through a non-wrapper: %q", path)
				}
			}
		})
	}
}

// The helper's own request goes to the caller's URL. A pattern that folds the URL local
// read it as "/api", a request to nowhere; it is dropped. A request in the helper that
// does not use the argument (a token refresh) is a real request and stays.
func TestLocalWrapper_HelperOwnRequestIsNotARoute(t *testing.T) {
	src := `
const API_BASE_URL = process.env.API_HOST;

const apiRequest = async (url: string, options: RequestInit = {}) => {
  const fullUrl = ` + "`${API_BASE_URL}/api${url}`" + `;
  const response = await fetch(fullUrl, options);
  if (response.status === 401) {
    await fetch('/api/auth/refresh', { method: 'POST' });
  }
  return response.json();
};

export const listOrders = () => apiRequest('/orders/list');
`
	got := clientRoutes(extractTS(t, src, "src/orders.ts"))
	if _, found := got["/api"]; found {
		t.Errorf("the helper's own request was emitted as /api: %+v", got)
	}
	if got["/api/auth/refresh"] != "POST" {
		t.Errorf("a real request inside the helper must stay: %+v", got)
	}
	if got["/api/orders/list"] != "GET" {
		t.Errorf("the call through the helper is missing: %+v", got)
	}
}

// An endpoint function also puts its parameter last, but the parameter is an id and
// its callers pass an id: `${base}/api/auth/verify-email/${confirmationId}`. Taking it
// for a helper dropped its own request, a real route. What makes a helper is a call
// that hands it a path; without one, nothing changes.
func TestLocalWrapper_EndpointFunctionIsNotAWrapper(t *testing.T) {
	src := `
async function processRegistrationConfirmation(confirmationId: string): Promise<void> {
  const response = await fetch(
    ` + "`${process.env.NEXT_PUBLIC_API_URL || 'http://localhost:8080'}/api/auth/verify-email/${confirmationId}`" + `,
    { method: 'POST' }
  );
}

export async function confirm(code: string) {
  await processRegistrationConfirmation(code);
}
`
	got := clientRoutes(extractTS(t, src, "src/lib/confirmations.ts"))
	if got["/api/auth/verify-email/{}"] != "POST" {
		t.Errorf("the endpoint function's own request must stay a route: %+v", got)
	}
}
