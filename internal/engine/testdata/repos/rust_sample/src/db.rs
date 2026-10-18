//! Data-access layer for the sample app.

pub struct User {
    pub id: u32,
    pub name: String,
}

impl User {
    pub fn new(id: u32) -> Self {
        User {
            id,
            name: String::from("sample"),
        }
    }
}

pub fn get_user(user_id: u32) -> User {
    User::new(user_id)
}

/// Talks to the database directly, so it is io_direct.
pub fn load_users(conn: &Connection) -> usize {
    conn.execute("SELECT * FROM users")
}
