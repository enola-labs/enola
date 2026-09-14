import { Injectable } from "@nestjs/common";
import { ResourceConnector } from "@example/sdk";

// The service that needs the gateway. It never makes a request itself: it reaches the
// gateway through the SDK's connector.
@Injectable()
export class CatalogService {
  constructor(private readonly resources: ResourceConnector) {}

  async items(): Promise<string[]> {
    return this.resources.getCatalogItems();
  }
}
