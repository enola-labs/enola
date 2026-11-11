export class Repo { save(): void { console.log("save"); } }
export class Helper { work(): void { console.log("work"); } }
export abstract class Base {
  protected helper: Helper;
  constructor() { this.helper = new Helper(); }
  run(): void { this.step(); }
  common(): void { this.helper.work(); }
  abstract step(): void;
}
