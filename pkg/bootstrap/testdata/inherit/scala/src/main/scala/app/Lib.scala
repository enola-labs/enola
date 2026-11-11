package app
class Repo { def save(): Unit = println("save") }
class Helper { def work(): Unit = println("work") }
abstract class Base {
  protected val helper: Helper = new Helper()
  def run(): Unit = step()
  def common(): Unit = helper.work()
  def step(): Unit
}
