package app;
public abstract class Base {
  protected Helper helper;
  public Base() { this.helper = new Helper(); }
  public void run() { step(); }
  public void common() { helper.work(); }
  protected abstract void step();
}
