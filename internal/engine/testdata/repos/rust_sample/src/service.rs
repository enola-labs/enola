//! Service layer: reaches the db module through its path, `db::load_users`, the
//! way Rust calls a function in another module. The call names its target, so it
//! resolves to src.load_users and list_users carries performs_io. `std::mem::take`
//! names the standard library and stays a bare call.

use crate::db;

pub fn list_users(conn: &Connection) -> usize {
    let mut buf = Vec::new();
    let _ = std::mem::take(&mut buf);
    db::load_users(conn)
}
