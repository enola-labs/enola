// A fully literal route. replica serves the same path, so a client calling it
// resolves to gateway only when an alias names gateway.
@Controller("/v1/catalog")
export class CatalogController {
  @Post("imports")
  startImport() {
    return { ok: true };
  }
}
