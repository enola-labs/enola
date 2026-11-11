namespace App;
public class Repo { public void Save() { System.Console.WriteLine("save"); } }
public class Helper { public void Work() { System.Console.WriteLine("work"); } }
public abstract class Base {
  protected Helper _helper;
  protected Base() { _helper = new Helper(); }
  public void Run() { Step(); }
  public void Common() { _helper.Work(); }
  protected abstract void Step();
}
