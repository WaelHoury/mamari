#include "repo.hpp"

class CppUserService : public CppBaseService {
private:
    CppUserRepo* repo;
public:
    CppUserService(CppUserRepo* r) : repo(r) {}

    int cpp_load(int id) {
        return repo->find_user_cpp(id);
    }

    int cpp_stack_load(int id) {
        CppUserRepo local_repo{id};
        return local_repo.find_user_cpp(id);
    }

    static CppUserService* cpp_factory();
};

CppUserService* CppUserService::cpp_factory() {
    return new CppUserService(new CppUserRepo());
}

int cpp_helper() {
    return 1;
}

int cpp_top_level() {
    return cpp_helper();
}
