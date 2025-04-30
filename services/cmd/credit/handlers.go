package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
)

func registerHttpHandlers(db DatabaseCredit) {
	http.HandleFunc("/credit", func(w http.ResponseWriter, r *http.Request) {
		handleCreateCredit(db, w, r)
	})
}

func handleCreateCredit(db DatabaseCredit, w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		var credit CreateCredit
		err := json.NewDecoder(r.Body).Decode(&credit)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
		} else if !credit.Validate() {
			slog.Error("Credit validation failed",
				slog.String("credit", credit.String()))
			w.WriteHeader(http.StatusBadRequest)
		}
		id, err := db.CreateCredit(credit)
		if err != nil {
			slog.Error("Failed to create credit",
				slog.String("credit", credit.String()),
				slog.String("error", err.Error()))
			w.WriteHeader(http.StatusInternalServerError)
		} else {
			w.Header().Set("X-Credit-Id", fmt.Sprintf("%d", id))
			w.WriteHeader(http.StatusCreated)
		}
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}
