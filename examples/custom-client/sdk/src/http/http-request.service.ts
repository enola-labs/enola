// The company's own HTTP abstraction. Nothing in its name or signature tells a tool
// that knows fetch, axios and Angular's HttpClient that it makes a request.
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
