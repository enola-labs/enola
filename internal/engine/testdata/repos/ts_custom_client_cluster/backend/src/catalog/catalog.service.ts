import { Injectable } from "@nestjs/common";
import { ResourceConnector } from "@example/sdk";

// The consumer that actually needs the gateway. It never makes a request itself: it
// reaches the gateway only through the SDK connector, which is why its edge to the
// gateway is transitive (backend -> sdk -> gateway) until connector attribution exists.
@Injectable()
export class CatalogService {
  constructor(private readonly resources: ResourceConnector) {}

  async items(): Promise<string[]> {
    return this.resources.getCatalogItems();
  }
}
