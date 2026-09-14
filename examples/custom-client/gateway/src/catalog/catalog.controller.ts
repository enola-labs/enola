@Controller("/v1/catalog")
export class CatalogController {
  @Post("imports")
  startImport() {
    return { ok: true };
  }
}
