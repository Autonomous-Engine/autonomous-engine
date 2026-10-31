package main

import "fmt"

// This file simulates consuming the exported types from pkg.
func main() {
	// Uses Repository interface and NewEntity function
	var repo Repository
	e := NewEntity("test")
	_ = repo.Save(nil, e)
	fmt.Println("done")
}
