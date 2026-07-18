package sample

type UserService struct {
	repo *UserRepo
}

func (service *UserService) Load(id string) string {
	return service.repo.Find(id)
}
