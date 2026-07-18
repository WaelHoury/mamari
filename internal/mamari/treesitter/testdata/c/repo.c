#include <stdio.h>

struct CUserRepo {
    int dummy;
};

char *c_find_user(struct CUserRepo *repo, int id) {
    return "user";
}
