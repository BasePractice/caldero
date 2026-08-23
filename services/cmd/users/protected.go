package main

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/ory/fosite"
)

// userInfo — то, что отдаётся владельцу токена. Раньше наружу уходили все
// claims сессии целиком.
type userInfo struct {
	Subject   string    `json:"sub"`
	Username  string    `json:"username,omitempty"`
	ExpiresAt time.Time `json:"exp"`
	Scopes    []string  `json:"scope"`
}

func handleUserInformation(w http.ResponseWriter, r *http.Request) {
	requester, ok := requesterFromContext(r.Context())
	if !ok {
		// Сюда можно попасть только в обход protect, но приведение типа
		// без проверки паниковало бы вместо ответа.
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	session := requester.GetSession()
	info := userInfo{
		Subject:   session.GetSubject(),
		Username:  session.GetUsername(),
		ExpiresAt: session.GetExpiresAt(fosite.AccessToken),
		Scopes:    requester.GetGrantedScopes(),
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(info); err != nil {
		slog.Error("Encoding user information", slog.String("err", err.Error()))
	}
}
