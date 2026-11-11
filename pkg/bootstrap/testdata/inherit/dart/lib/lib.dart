class Repo { void save() { print("save"); } }
class Helper { void work() { print("work"); } }
abstract class Base {
  final Helper helper = Helper();
  void run() { step(); }
  void common() { helper.work(); }
  void step();
}
