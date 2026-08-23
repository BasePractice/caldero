package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// TestUsersContacts проверяет чтение контактов из сервиса пользователей:
// хранить их вторую копию здесь нельзя — она разойдётся с профилем
// при первой же смене адреса.
func TestUsersContacts(t *testing.T) {
	user := uuid.New()
	serviceId := uuid.New()

	var path, authorized, roles string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		authorized = r.Header.Get("X-Authorized-Id")
		roles = r.Header.Get("X-Roles")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"user_id":"` + user.String() + `","email":"user@example.com",` +
			`"email_confirmed":true,"phone":"+79001112233","phone_confirmed":false}`))
	}))
	defer server.Close()

	contact, err := NewUsersContacts(server.URL, serviceId).Contacts(t.Context(), user)
	if err != nil {
		t.Fatalf("чтение контактов: %v", err)
	}

	if !strings.HasSuffix(path, "/users/"+user.String()+"/contacts") {
		t.Errorf("путь %q", path)
	}
	// Контакты — персональные данные, и отдаются только оператору.
	if authorized != serviceId.String() || roles != "operator" {
		t.Errorf("вызов ушёл от %q с ролями %q", authorized, roles)
	}
	if contact.Email != "user@example.com" || !contact.EmailConfirmed {
		t.Errorf("контакты %+v", contact)
	}
}

func TestUsersContactsErrors(t *testing.T) {
	user := uuid.New()

	t.Run("сервис не настроен", func(t *testing.T) {
		if _, err := NewUsersContacts("", uuid.New()).Contacts(t.Context(), user); err == nil {
			t.Fatal("пустой адрес принят")
		}
	})

	t.Run("сервис отказал", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "нет", http.StatusNotFound)
		}))
		defer server.Close()

		_, err := NewUsersContacts(server.URL, uuid.New()).Contacts(t.Context(), user)
		if err == nil {
			t.Fatal("отказ сервиса принят за успех")
		}
	})

	t.Run("сервис недоступен", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		endpoint := server.URL
		server.Close()

		if _, err := NewUsersContacts(endpoint, uuid.New()).Contacts(t.Context(), user); err == nil {
			t.Fatal("недоступный сервис принят за успех")
		}
	})

	t.Run("ответ не разбирается", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`не json`))
		}))
		defer server.Close()

		if _, err := NewUsersContacts(server.URL, uuid.New()).Contacts(t.Context(), user); err == nil {
			t.Fatal("неразбираемый ответ принят за успех")
		}
	})
}
