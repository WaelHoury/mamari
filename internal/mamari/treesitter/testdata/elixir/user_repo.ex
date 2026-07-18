defmodule ExUserRepo do
  def find_user_ex(id) do
    id
  end

  defp helper_ex(x) do
    x
  end

  def load_ex(id) do
    helper_ex(id)
  end

  def guarded_ex(id) when is_integer(id) do
    id
  end
end
