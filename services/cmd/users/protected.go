package main

import (
	"encoding/json"
	"net/http"

	"github.com/ory/fosite/token/jwt"
)

func handleUserInformation(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	claims := ctx.Value("claims").(jwt.MapClaims)
	bytes, err := json.Marshal(claims)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	_, err = w.Write(bytes)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}
