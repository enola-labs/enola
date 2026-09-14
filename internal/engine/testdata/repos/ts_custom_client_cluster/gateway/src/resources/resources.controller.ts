// A parameterized controller prefix. A client calling /v1/resources/catalog/items
// reaches this handler at runtime, but only a literal-against-parameter match can
// say so, and that match is opt-in.
@Controller("/v1/resources/:type")
export class ResourcesController {
  @Get("items")
  listItems() {
    return [];
  }
}
