<?php
namespace App;
class Concrete extends Base {
    private Repo $repo;
    public function __construct() { parent::__construct(); $this->repo = new Repo(); }
    protected function step(): void { $this->repo->save(); }
    public function handle(): void { $this->common(); $this->helper->work(); }
}
