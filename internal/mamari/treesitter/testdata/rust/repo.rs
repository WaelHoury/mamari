pub struct UserRepo {}

impl UserRepo {
    pub fn find_user(&self, id: &str) -> String {
        id.to_string()
    }
}
