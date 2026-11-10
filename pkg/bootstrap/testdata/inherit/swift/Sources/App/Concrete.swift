class Concrete: Base {
    let repo = Repo()
    override func step() { repo.save() }
    func handle() { common(); self.helper.work() }
}

@main
struct AppMain {
    static func main() {
        let c = Concrete()
        c.run()
        c.handle()
    }
}
