// Every request in this file goes through a helper declared here, which puts its
// argument at the end of the URL after "/api". No client pattern names the helper, so
// these calls used to be invisible and the routes they reach read as unused. The
// wrapper pass reads them as requests to "/api" + the argument.
const API_BASE_URL = process.env.API_HOST;

const apiRequest = async (url: string, options: RequestInit = {}) => {
  const fullUrl = `${API_BASE_URL}/api${url}`;
  const response = await fetch(fullUrl, options);
  return response.json();
};

export async function listOrdersThroughHelper() {
  return apiRequest("/orders/list");
}

export async function createOrderThroughHelper(body: unknown) {
  return apiRequest("/orders/create", { method: "POST", body: JSON.stringify(body) });
}
