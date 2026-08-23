package main

import (
	"net/http"
)

func registerHttpHandlers(service *Service) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /register", service.handleRegister)
	mux.HandleFunc("GET /auth", service.handleAuthorization)
	mux.HandleFunc("POST /auth", service.handleAuthorization)
	mux.HandleFunc("POST /token", service.handleToken)
	mux.HandleFunc("GET /me", service.protect(handleUserInformation))
	mux.HandleFunc("GET /.well-known/jwks.json", service.handleJWKS)
	mux.HandleFunc("POST /clients", service.handleCreateClient)
	mux.HandleFunc("POST /rotate-keys", service.handleRotateKeys)
	mux.HandleFunc("POST /revoke", service.handleRevoke)
	return mux
}
