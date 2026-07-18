from repos.primary import UserRepo as PrimaryRepo

class UserService:
    def __init__(self, repo: PrimaryRepo):
        self.repo = repo

    def load(self, user_id: str) -> str:
        return self.repo.find_user(user_id)

def load_with(repo: PrimaryRepo, user_id: str) -> str:
    return repo.find_user(user_id)
