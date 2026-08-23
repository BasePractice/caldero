//go:build integration

package main

import (
	"database/sql"
	"testing"
)

// rawDB достаёт соединение из репозитория: часть проверок удобнее делать
// прямым SQL, не заводя ради них методов в интерфейсе.
func rawDB(t *testing.T, database Database) *sql.DB {
	t.Helper()
	store, ok := database.(*ds)
	if !ok {
		t.Fatalf("неожиданный тип репозитория %T", database)
	}
	return store.db
}
