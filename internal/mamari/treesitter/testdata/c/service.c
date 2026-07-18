struct CUserRepo;

struct CUserService {
    struct CUserRepo *repo;
};

char *c_load(struct CUserService *svc, int id) {
    return c_find_user(svc->repo, id);
}

char *c_helper(void) {
    return "hi";
}

char *c_top_level(void) {
    return c_helper();
}
