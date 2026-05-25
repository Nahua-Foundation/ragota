from models.base import Repository


class User:
    def __init__(self, id: int, name: str, email: str):
        self.id = id
        self.name = name
        self.email = email

    def to_dict(self) -> dict:
        return {"id": self.id, "name": self.name, "email": self.email}


class UserRepository(Repository):
    def __init__(self):
        self._store: dict[int, User] = {}

    def find_by_id(self, id: int) -> dict:
        user = self._store.get(id)
        return user.to_dict() if user else {}

    def save(self, entity: dict) -> None:
        user = User(entity["id"], entity["name"], entity["email"])
        self._store[user.id] = user
