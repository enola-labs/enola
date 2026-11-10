<?php
namespace App;
class Repo { public function save(): void { echo "save"; } }
class Helper { public function work(): void { echo "work"; } }
abstract class Base {
    protected Helper $helper;
    public function __construct() { $this->helper = new Helper(); }
    public function run(): void { $this->step(); }
    public function common(): void { $this->helper->work(); }
    abstract protected function step(): void;
}
