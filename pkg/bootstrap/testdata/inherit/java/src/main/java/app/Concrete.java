package app;
public class Concrete extends Base {
  private final Repo repo = new Repo();
  @Override protected void step() { repo.save(); }
  public void handle() { common(); this.helper.work(); }
  public static void main(String[] args) {
    Concrete c = new Concrete();
    c.run();
    c.handle();
  }
}
