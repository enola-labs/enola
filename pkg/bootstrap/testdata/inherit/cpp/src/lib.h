#pragma once
#include <cstdio>
class Repo { public: void save() { std::puts("save"); } };
class Helper { public: void work() { std::puts("work"); } };
class Base {
 public:
  Base() : helper_(new Helper()) {}
  virtual ~Base() {}
  void run() { step(); }
  void common() { helper_->work(); }
 protected:
  virtual void step() = 0;
  Helper* helper_;
};
