pub struct Repo;
impl Repo { pub fn save(&self) { println!("save"); } }
pub struct Helper;
impl Helper { pub fn work(&self) { println!("work"); } }
pub trait Base {
    fn helper(&self) -> &Helper;
    fn step(&self);
    fn run(&self) { self.step(); }
    fn common(&self) { self.helper().work(); }
}
pub mod other;
