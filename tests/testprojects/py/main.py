from services.user_service import UserService


def main():
    svc = UserService()
    user = svc.create_user("Alice", "alice@example.com")
    print(f"Created user: {user.name}")

    found = svc.get_user(1)
    if found:
        print(f"Found user: {found.email}")


if __name__ == "__main__":
    main()
