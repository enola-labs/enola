import { Injectable } from "@nestjs/common";
import { GET, HttpMethod, IHttpRequestService } from "../http/http-request.service";

// A connector shipped in a shared SDK. Every call goes through the injected
// IHttpRequestService, naming the target service by a string field rather than a
// host. The service name deliberately matches no repository label, so resolving
// it needs a declared alias rather than a lucky substring.
@Injectable()
export class ResourceConnector {
  private readonly serviceName = "resource-api";
  private readonly basePath: string = "/v1/resources/";

  constructor(private readonly httpRequestService: IHttpRequestService) {}

  // The path folds through a class field and a local const, and the verb is a bare
  // imported constant. Served by gateway under a :type parameter only.
  async getCatalogItems(): Promise<string[]> {
    const url = `${this.basePath}catalog/items`;
    return this.httpRequestService.sendRequest<string[]>(this.serviceName, url, { method: GET });
  }

  // A fully literal path with an enum-member verb. Served by BOTH gateway and
  // replica, so only a declared alias may choose between them.
  async startImport(body: unknown): Promise<void> {
    await this.httpRequestService.sendRequest<void>(this.serviceName, "/v1/catalog/imports", {
      method: HttpMethod.POST,
      body,
    });
  }

  // The path is a method's return value. Nothing may be derived from it: it must be
  // counted as skipped, never guessed.
  async getByKey(key: string): Promise<string> {
    return this.httpRequestService.sendRequest<string>(this.serviceName, this.pathFor(key), { method: GET });
  }

  private pathFor(key: string): string {
    return `/v1/resources/${key}`;
  }
}
