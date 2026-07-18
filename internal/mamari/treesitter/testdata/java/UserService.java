package sample;

class UserService {
    private final UserRepo repo = new UserRepo();

    String load(String id) {
        return repo.find(id);
    }
}
