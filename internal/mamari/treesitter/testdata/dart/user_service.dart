class DartUserService {
  DartUserRepo repo;
  DartUserService(this.repo);

  String loadDart(int id) {
    return repo.findUserDart(id);
  }
}

String topLevelDart() {
  return helperTopLevelDart();
}

String helperTopLevelDart() {
  return "hi";
}
