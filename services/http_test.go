package services

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type request struct {
	Month uint   `json:"month"`
	Name  string `json:"name"`
}

func TestDecodeJSON(t *testing.T) {
	recorder := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"month":36,"name":"кредит"}`))

	got, err := DecodeJSON[request](recorder, r)
	if err != nil {
		t.Fatalf("разбор тела: %v", err)
	}
	if got.Month != 36 || got.Name != "кредит" {
		t.Errorf("получено %+v", got)
	}
}

// TestDecodeJSONUnknownField фиксирует причину, по которой поля проверяются
// строго: опечатка в имени иначе превращается в нулевое значение, и кредит
// оформляется на нулевой срок.
func TestDecodeJSONUnknownField(t *testing.T) {
	recorder := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"mounth":36}`))

	if _, err := DecodeJSON[request](recorder, r); err == nil {
		t.Fatal("опечатка в имени поля принята")
	}
}

func TestDecodeJSONMalformed(t *testing.T) {
	recorder := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"month":`))

	_, err := DecodeJSON[request](recorder, r)
	if err == nil {
		t.Fatal("оборванное тело принято")
	}

	WriteDecodeError(recorder, err)
	if recorder.Code != http.StatusBadRequest {
		t.Errorf("код ответа %d, ожидался %d", recorder.Code, http.StatusBadRequest)
	}
}

// TestDecodeJSONTooLarge проверяет и ограничение размера, и подбор статуса:
// превышение лимита это 413, а не 400, иначе клиент чинит не то.
func TestDecodeJSONTooLarge(t *testing.T) {
	body := `{"name":"` + strings.Repeat("я", MaxRequestBody) + `"}`
	recorder := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))

	_, err := DecodeJSON[request](recorder, r)
	if err == nil {
		t.Fatal("тело больше предела принято")
	}

	WriteDecodeError(recorder, err)
	if recorder.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("код ответа %d, ожидался %d", recorder.Code, http.StatusRequestEntityTooLarge)
	}
}
