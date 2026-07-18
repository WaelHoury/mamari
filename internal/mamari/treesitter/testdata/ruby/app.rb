require 'json'

class UserService
  def initialize(repo)
    @repo = repo
  end

  def load(id)
    @repo.find_user(id)
  end

  def self.factory
    new(Repo.new)
  end
end

module Helpers
  def self.greet
    helper()
  end
end

def helper
  "hi"
end
