import { Base, Repo } from "./lib";
export class Concrete extends Base {
  private repo = new Repo();
  step(): void { this.repo.save(); }
  handle(): void { this.common(); this.helper.work(); }
}
export function main(): void {
  const c = new Concrete();
  c.run();
  c.handle();
}
main();
