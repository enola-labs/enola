package main

import "example.com/fx/app"

func main() {
	c := app.NewConcrete()
	c.Common()
	c.Handle()
}
