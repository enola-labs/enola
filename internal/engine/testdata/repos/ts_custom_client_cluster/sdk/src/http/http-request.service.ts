// The in-house HTTP abstraction. Nothing in its name or signature says HTTP to a
// tool that only knows fetch, axios and Angular's HttpClient, which is the whole
// problem this fixture exists to pin.
export enum HttpMethod {
  GET = "GET",
  POST = "POST",
}

export const GET = HttpMethod.GET;

export interface RequestOptions {
  method: HttpMethod;
  body?: unknown;
}

export interface IHttpRequestService {
  sendRequest<T>(serviceName: string, path: string, options: RequestOptions): Promise<T>;
}
