import { Injectable } from "@nestjs/common";
import { GET, HttpMethod, IHttpRequestService } from "../http/http-request.service";

// A connector shipped in a shared SDK. It names the service it calls by a string field,
// and builds its paths from a base path field.
@Injectable()
export class ResourceConnector {
  private readonly serviceName = "resource-api";
  private readonly basePath: string = "/v1/resources/";

  constructor(private readonly httpRequestService: IHttpRequestService) {}

  // The path is built at runtime from a field and a local const.
  async getCatalogItems(): Promise<string[]> {
    const url = `${this.basePath}catalog/items`;
    return this.httpRequestService.sendRequest<string[]>(this.serviceName, url, { method: GET });
  }

  // A literal path.
  async startImport(body: unknown): Promise<void> {
    await this.httpRequestService.sendRequest<void>(this.serviceName, "/v1/catalog/imports", {
      method: HttpMethod.POST,
      body,
    });
  }

  // The path is whatever a method returns. There is nothing to read here.
  async getByKey(key: string): Promise<string> {
    return this.httpRequestService.sendRequest<string>(this.serviceName, this.pathFor(key), { method: GET });
  }

  private pathFor(key: string): string {
    return `/v1/resources/${key}`;
  }
}
