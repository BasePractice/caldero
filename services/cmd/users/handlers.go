package main

import (
	"net/http"

	"wish/services"
)

func registerHttpHandlers(service *Service) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /register", service.handleRegister)
	mux.HandleFunc("GET /auth", service.handleAuthorization)
	mux.HandleFunc("POST /auth", service.handleAuthorization)
	mux.HandleFunc("POST /token", service.handleToken)
	mux.HandleFunc("GET /me", service.protect(handleUserInformation))
	mux.HandleFunc("GET /profile", service.protect(service.handleProfile))
	mux.HandleFunc("PATCH /profile", service.protect(service.handleUpdateProfile))
	mux.HandleFunc("POST /profile/confirmations", service.protect(service.handleRequestConfirmation))
	mux.HandleFunc("POST /profile/confirmations/verify", service.protect(service.handleVerifyConfirmation))
	mux.HandleFunc("GET /auth/social/{provider}", service.handleSocialStart)
	mux.HandleFunc("GET /auth/social/{provider}/callback", service.handleSocialCallback)
	mux.HandleFunc("GET /profile/identities", service.protect(service.handleIdentities))
	mux.HandleFunc("POST /profile/identities/{provider}", service.protect(service.handleLinkIdentity))
	mux.HandleFunc("DELETE /profile/identities/{provider}", service.protect(service.handleUnlinkIdentity))
	mux.HandleFunc("GET /users/{id}", service.handlePublicProfile)
	mux.HandleFunc("GET /.well-known/jwks.json", service.handleJWKS)
	mux.HandleFunc("POST /clients", service.handleCreateClient)
	mux.HandleFunc("POST /rotate-keys", service.handleRotateKeys)
	mux.HandleFunc("POST /revoke", service.handleRevoke)
	return services.Measure("users", mux)
}
