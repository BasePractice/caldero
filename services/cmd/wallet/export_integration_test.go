//go:build integration

package main

import (
	"database/sql"
	"testing"
)

func rawDB(t *testing.T, database DatabaseWallet) *sql.DB {
	t.Helper()
	store, ok := database.(*ds)
	if !ok {
		t.Fatalf("неожиданный тип репозитория %T", database)
	}
	return store.db
}
