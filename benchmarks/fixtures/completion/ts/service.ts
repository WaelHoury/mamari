import { UserRepo as PrimaryRepo } from './repos/primary'

export class UserService {
  constructor(private readonly repo: PrimaryRepo) {}
  load(id: string) { return this.repo.findUser(id) }
}

export function loadWith(repo: PrimaryRepo, id: string) {
  const alias = repo
  return alias.findUser(id)
}
