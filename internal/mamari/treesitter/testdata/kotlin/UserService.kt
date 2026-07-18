class KtUserService(private val repo: KtUserRepo) {
    fun ktLoad(id: Int): String {
        return repo.findUserKt(id)
    }
}

interface KtGreeter {
    fun greet(): String
}

fun ktHelper(): String {
    return "hi"
}

fun ktTopLevel(): String {
    return ktHelper()
}
