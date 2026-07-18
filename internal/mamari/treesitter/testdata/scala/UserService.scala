class ScUserService(private val repo: ScUserRepo) {
  def scLoad(id: Int): String = {
    repo.findUserSc(id)
  }
}

trait ScGreeter {
  def greet(): String
}

object ScUserService {
  def scFactory(): ScUserService = {
    new ScUserService(new ScUserRepo())
  }
}

def scHelper(): String = "hi"

def scTopLevel(): String = scHelper()
