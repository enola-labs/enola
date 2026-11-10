class Repo { func save() { print("save") } }
class Helper { func work() { print("work") } }
class Base {
    let helper = Helper()
    func run() { step() }
    func common() { helper.work() }
    func step() { fatalError("abstract") }
}
