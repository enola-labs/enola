// GENERATED CODE - DO NOT MODIFY BY HAND
import 'package:http/http.dart' as http;

class PetsClient {
  final http.Client client;
  PetsClient(this.client);

  Future<void> getPet() async {
    await client.get(Uri.parse('/pets/detail'));
  }
}
