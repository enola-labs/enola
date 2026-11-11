package app

class Repo {
    fun save() {
        println("save")
    }
}

class Helper {
    fun work() {
        println("work")
    }
}

abstract class Base {
    protected val helper = Helper()

    fun run() {
        step()
    }

    fun common() {
        helper.work()
    }

    abstract fun step()
}
