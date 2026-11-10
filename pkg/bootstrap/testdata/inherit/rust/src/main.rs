use fx::{Base, Helper, Repo};
struct Concrete { helper: Helper, repo: Repo }
impl Base for Concrete {
    fn helper(&self) -> &Helper { &self.helper }
    fn step(&self) { self.repo.save(); }
}
impl Concrete {
    fn handle(&self) { self.common(); self.helper.work(); }
}
fn main() {
    let c = Concrete { helper: Helper, repo: Repo };
    c.run();
    c.handle();
}
