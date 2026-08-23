package services

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
)

// MaxRequestBody ограничивает размер тела запроса: json.Decoder по умолчанию
// читает столько, сколько пришлёт клиент.
const MaxRequestBody = 1 << 20 // 1 МиБ

// DecodeJSON читает тело запроса в значение типа T.
func DecodeJSON[T any](w http.ResponseWriter, r *http.Request) (T, error) {
	var v T
	r.Body = http.MaxBytesReader(w, r.Body, MaxRequestBody)

	decoder := json.NewDecoder(r.Body)
	// Опечатка в имени поля должна быть ошибкой, а не молчаливым нулевым
	// значением: иначе "mounth": 36 создаёт кредит на нулевой срок.
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&v); err != nil {
		return v, fmt.Errorf("decoding request body: %w", err)
	}
	return v, nil
}

// WriteDecodeError подбирает статус под причину: превышение лимита это 413,
// а не 400.
func WriteDecodeError(w http.ResponseWriter, err error) {
	var maxBytes *http.MaxBytesError
	if errors.As(err, &maxBytes) {
		http.Error(w, "Request body is too large", http.StatusRequestEntityTooLarge)
		return
	}
	http.Error(w, "Malformed request body", http.StatusBadRequest)
}
