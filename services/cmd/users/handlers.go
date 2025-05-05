package main

import (
	"context"
	"net/http"
)

func registerHttpHandlers(_ context.Context, service *Service) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /register", service.handleRegister)
	mux.HandleFunc("/auth", service.handleAuthorization)
	mux.HandleFunc("/token", service.handleToken)
	mux.HandleFunc("/me", service.protect(handleUserInformation))
	mux.HandleFunc("/.well-known/jwks.json", service.handleJWKS)
	mux.HandleFunc("/rotate-keys", service.handleRotateKeys)
	mux.HandleFunc("/revoke", service.handleRevoke)
	return mux
}
