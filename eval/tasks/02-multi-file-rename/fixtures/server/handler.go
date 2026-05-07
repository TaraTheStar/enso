package server

import "example.com/renametest/greeter"

func Handle(name string) string {
	return greeter.Greet(name)
}
