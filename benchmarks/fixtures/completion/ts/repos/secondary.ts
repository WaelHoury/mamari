export class UserRepo {
  findUser(id: string) { return `secondary:${id}` }
}
