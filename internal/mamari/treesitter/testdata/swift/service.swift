class UserService {
    func load() {
        helper()
    }

    func greet() {
        print("hi")
    }
}

protocol Greeter {
    func greet()
}

func helper() {
    print("helping")
}
