use crate::{Base, Helper};
pub struct Other { pub helper: Helper }
impl Base for Other {
    fn helper(&self) -> &Helper { &self.helper }
    fn step(&self) { println!("other"); }
}
