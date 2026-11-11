#include "lib.h"
class Other : public Base {
 protected:
  void step() override { std::puts("other"); }
};
