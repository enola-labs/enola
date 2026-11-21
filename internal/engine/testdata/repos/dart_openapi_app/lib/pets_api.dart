import 'package:http/http.dart' as http;

// The app's only HTTP is this client: it serves nothing, so the spec beside it is
// one it calls.
class PetsApi {
  final http.Client client;
  PetsApi(this.client);

  Future<void> listPets() async {
    await client.get(Uri.parse('/pets'));
  }
}
