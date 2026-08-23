package services

import (
	"errors"

	"github.com/lib/pq"
)

// uniqueViolation — код нарушения уникальности в PostgreSQL.
const uniqueViolation = "23505"

// IsUniqueViolation отличает конфликт данных от сбоя БД: первый — это ответ
// 409 клиенту, второй — 500. Без такого разделения клиент на попытку создать
// дубликат получал внутреннюю ошибку.
func IsUniqueViolation(err error) bool {
	var pgErr *pq.Error
	return errors.As(err, &pgErr) && pgErr.Code == uniqueViolation
}
