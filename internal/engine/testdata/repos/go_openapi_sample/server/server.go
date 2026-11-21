package server

import "net/http"

// Server implements the operations of api/openapi/petstore.yaml by name.
type Server struct{}

func (s *Server) ListPets(w http.ResponseWriter, r *http.Request) {}

func (s *Server) GetPet(w http.ResponseWriter, r *http.Request, id string) {}
