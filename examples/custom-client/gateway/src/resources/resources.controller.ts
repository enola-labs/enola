// The resource type is a path parameter. A call to /v1/resources/catalog/items reaches
// this handler at runtime.
@Controller("/v1/resources/:type")
export class ResourcesController {
  @Get("items")
  listItems() {
    return [];
  }
}
