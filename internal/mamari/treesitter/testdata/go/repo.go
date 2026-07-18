package sample

type UserRepo struct{}

func (repo *UserRepo) Find(id string) string {
	return id
}
