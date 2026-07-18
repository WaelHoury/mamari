local LuaUserRepo = {}

function LuaUserRepo.new()
  return setmetatable({}, LuaUserRepo)
end

function LuaUserRepo:findUserLua(id)
  return id
end

return LuaUserRepo
