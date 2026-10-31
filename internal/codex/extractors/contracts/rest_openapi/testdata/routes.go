package main

import (
	"net/http"

	"github.com/gorilla/mux"
)

func main() {
	r := mux.NewRouter()
	r.HandleFunc("/api/users", handleUsers)
	r.HandleFunc("/api/users/{id}", handleUserByID)
	http.ListenAndServe(":8080", r)
}

func handleUsers(w http.ResponseWriter, r *http.Request)    {}
func handleUserByID(w http.ResponseWriter, r *http.Request) {}
