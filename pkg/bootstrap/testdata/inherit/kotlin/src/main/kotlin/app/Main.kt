package app
class Concrete : Base() {
    private val repo = Repo()
    override fun step() { repo.save() }
    fun handle() { common(); this.helper.work() }
}
fun main() {
    val c = Concrete()
    c.run()
    c.handle()
}
