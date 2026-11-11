namespace App;
public class Concrete : Base {
  private readonly Repo _repo = new Repo();
  protected override void Step() { _repo.Save(); }
  public void Handle() { Common(); this._helper.Work(); }
}
public static class Program {
  public static void Main(string[] args) {
    var c = new Concrete();
    c.Run();
    c.Handle();
  }
}
