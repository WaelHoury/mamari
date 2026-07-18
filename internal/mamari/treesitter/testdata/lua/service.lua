local LuaUserRepo = require("repo")

local LuaUserService = {}

function LuaUserService:loadLua(id)
  local repo = LuaUserRepo.new()
  return repo:findUserLua(id)
end

local function luaHelper()
  return "hi"
end

local function luaTopLevel()
  return luaHelper()
end

return LuaUserService
