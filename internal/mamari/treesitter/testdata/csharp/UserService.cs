namespace Sample;

class UserService
{
    private readonly UserRepo repo = new UserRepo();

    public string Load(string id)
    {
        return repo.Find(id);
    }
}
