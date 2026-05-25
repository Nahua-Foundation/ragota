from models.user import UserRepository, User


class UserService:
    def __init__(self):
        self.repo = UserRepository()

    def get_user(self, id: int) -> User | None:
        data = self.repo.find_by_id(id)
        if not data:
            return None
        return User(data["id"], data["name"], data["email"])

    def create_user(self, name: str, email: str) -> User:
        user = User(id=1, name=name, email=email)
        self.repo.save(user.to_dict())
        return user
