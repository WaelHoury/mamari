use crate::repo::UserRepo;

pub struct UserService {
    repo: UserRepo,
}

impl UserService {
    pub fn new(repo: UserRepo) -> Self {
        UserService { repo }
    }

    pub fn load(&self, id: &str) -> String {
        self.repo.find_user(id)
    }
}

pub trait Greeter {
    fn greet(&self) -> String;
}

impl Greeter for UserService {
    fn greet(&self) -> String {
        helper()
    }
}

fn helper() -> String {
    String::new()
}
