import 'package:fx/lib.dart';
class Concrete extends Base {
  final Repo repo = Repo();
  @override
  void step() { repo.save(); }
  void handle() { common(); this.helper.work(); }
}
void main() {
  final c = Concrete();
  c.run();
  c.handle();
}
