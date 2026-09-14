// The ambiguity control. Serves exactly the literal route gateway serves, so a
// client call to it has two candidate providers and no edge may be drawn unless
// something the user declared picks one.
@Controller("/v1/catalog")
export class CatalogReplicaController {
  @Post("imports")
  startImport() {
    return { ok: true };
  }
}
