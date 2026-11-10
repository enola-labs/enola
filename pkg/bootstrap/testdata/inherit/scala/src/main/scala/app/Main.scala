package app
class Concrete extends Base {
  private val repo: Repo = new Repo()
  override def step(): Unit = repo.save()
  def handle(): Unit = { common(); this.helper.work() }
}
object Main {
  def main(args: Array[String]): Unit = {
    val c: Concrete = new Concrete()
    c.run()
    c.handle()
  }
}
