class UserRepo:
    def find_user(self, user_id: str) -> str:
        return f"primary:{user_id}"
