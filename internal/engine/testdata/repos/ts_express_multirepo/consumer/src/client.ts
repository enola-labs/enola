import axios from "axios";

// The consumer half, and the reason receiver binding matters: a client call and a
// route registration are the same text apart from the receiver, so only the binding
// tells them apart. These must all stay client calls; the server repo's
// registrations must not be reported as outbound calls, and these must not be
// reported as routes served here.
//
// Note the deliberate absence of a worked example in this comment. The client
// patterns scan raw source with no comment awareness, so spelling one out here would
// mint a phantom route — latent on the corpus (zero occurrences), but real.
export async function health() {
  return axios.get("/healthcheck");
}

export async function listAdminUsers() {
  return axios.get("/admin/users");
}

export async function banUser(id: string) {
  return axios.post(`/admin/users/${id}/ban`, {});
}

// The cross-file mount, from the client side. This call is the reason the server's
// composed path has to be right: '/webhooks/login' matches the route only if the
// mount prefix in index.js reached the registration in routes/webhooks.js. While
// those routes went unemitted, this was an unresolved edge.
export async function login(body: unknown) {
  return axios.post("/webhooks/login", body);
}

// No server in this snapshot serves this one, so it stays an unresolved edge — the
// control showing the linker is matching on real paths rather than accepting
// anything.
export async function unknown() {
  return axios.get("/not/served/anywhere");
}

const statusUrl = `${process.env.API_HOST}/healthcheck`;

export async function pollStatus() {
  return fetch(statusUrl, { method: "GET" });
}

export async function metricsTail(window: string) {
  return axios.get(`${process.env.API_HOST}/admin/users`);
}

// Two mounts deep on the server: '/api' in index.js, '/orders' on its apiRouter, the
// route itself in routes/orders.js. It resolves only if both prefixes reached it.
export async function listOrders() {
  return axios.get("/api/orders/list");
}
