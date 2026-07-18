defmodule ExUserService do
  alias ExUserRepo, as: RepoAlias

  def load_user_ex(id) do
    RepoAlias.find_user_ex(id)
  end
end
