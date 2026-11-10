#include "lib.h"
class Concrete : public Base {
 public:
  void handle() { common(); this->helper_->work(); }
 protected:
  void step() override { repo_.save(); }
 private:
  Repo repo_;
};
int main() {
  Concrete c;
  c.run();
  c.handle();
  return 0;
}
