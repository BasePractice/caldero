package main

import (
	"context"
	"net/http"
)

func registerHttpHandlers(_ context.Context, service *Service) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /register", service.handleRegister)
	mux.HandleFunc("POST /token", service.handleToken)
	mux.HandleFunc("GET /me", service.protect(handleUserInformation))
	mux.HandleFunc("GET /.well-known/jwks.json", service.handleJWKS)
	mux.HandleFunc("POST /rotate-keys", service.handleRotateKeys)
	mux.HandleFunc("POST /revoke", service.handleRevoke)
	return mux
}
