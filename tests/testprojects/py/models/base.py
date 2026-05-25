from abc import ABC, abstractmethod


class Repository(ABC):
    @abstractmethod
    def find_by_id(self, id: int) -> dict:
        ...

    @abstractmethod
    def save(self, entity: dict) -> None:
        ...
