class UserService:
    def __init__(self, repo):
        self.repo = repo

    def load(self, id):
        return self.repo.find_user(id)


def helper():
    return "hi"


def top_level():
    return helper()
