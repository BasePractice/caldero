package main

import (
	"encoding/json"
	"net/url"
	"testing"
	"time"

	"github.com/ory/fosite"
	"github.com/ory/fosite/handler/openid"
	"github.com/ory/fosite/token/jwt"
)

// TestTokenSessionRoundTrip проверяет то, что раньше не работало вовсе:
// сохранённая сессия должна читаться обратно. Прежний код сериализовал
// fosite.Request целиком, и разбор падал на интерфейсных полях Client
// и Session, поэтому introspection и refresh не работали никогда.
func TestTokenSessionRoundTrip(t *testing.T) {
	expires := time.Date(2026, time.March, 1, 10, 0, 0, 0, time.UTC)
	client := &fosite.DefaultClient{ID: "test-client", Scopes: []string{"openid", "read", "write"}}

	source := fosite.NewRequest()
	source.ID = "request-42"
	source.RequestedAt = time.Date(2026, time.March, 1, 9, 0, 0, 0, time.UTC)
	source.Client = client
	source.RequestedScope = fosite.Arguments{"openid", "read", "write"}
	source.GrantedScope = fosite.Arguments{"openid", "read"}
	source.RequestedAudience = fosite.Arguments{"wish"}
	source.GrantedAudience = fosite.Arguments{"wish"}
	source.Form = url.Values{"grant_type": []string{"password"}}
	session := &openid.DefaultSession{
		Subject: "1b1cb3d0-0a29-4b3f-9d2f-6f5c1f52a001",
		Claims:  &jwt.IDTokenClaims{Subject: "1b1cb3d0-0a29-4b3f-9d2f-6f5c1f52a001"},
		Headers: new(jwt.Headers),
	}
	session.SetExpiresAt(fosite.AccessToken, expires)
	source.Session = session

	data, err := encodeTokenSession(source)
	if err != nil {
		t.Fatalf("не удалось закодировать сессию: %v", err)
	}

	restoredSession := new(openid.DefaultSession)
	restored, clientId, err := decodeTokenSession(data, restoredSession, client)
	if err != nil {
		t.Fatalf("не удалось декодировать сессию: %v", err)
	}

	if clientId != "test-client" {
		t.Errorf("client_id = %q, ожидался test-client", clientId)
	}
	if restored.GetID() != source.GetID() {
		t.Errorf("id = %q, ожидался %q", restored.GetID(), source.GetID())
	}
	if !restored.GetRequestedAt().Equal(source.GetRequestedAt()) {
		t.Errorf("requested_at = %s, ожидалось %s", restored.GetRequestedAt(), source.GetRequestedAt())
	}
	// Granted scope критичен: без него проверка прав на защищённых
	// эндпоинтах пропускает всё подряд.
	if got := restored.GetGrantedScopes(); !got.Has("openid", "read") || got.Has("write") {
		t.Errorf("granted scope = %v, ожидались openid и read без write", got)
	}
	if got := restored.GetRequestedScopes(); !got.Has("write") {
		t.Errorf("requested scope = %v, ожидался write", got)
	}
	if got := restored.GetRequestForm().Get("grant_type"); got != "password" {
		t.Errorf("form grant_type = %q, ожидался password", got)
	}
	if got := restored.GetSession().GetSubject(); got != session.GetSubject() {
		t.Errorf("subject = %q, ожидался %q", got, session.GetSubject())
	}
	if got := restored.GetSession().GetExpiresAt(fosite.AccessToken); !got.Equal(expires) {
		t.Errorf("expires_at = %s, ожидалось %s", got, expires)
	}
}

// TestFositeRequestIsNotSerializable фиксирует причину, по которой понадобился
// собственный формат хранения: наивная сериализация fosite.Request не работает.
func TestFositeRequestIsNotSerializable(t *testing.T) {
	source := fosite.NewRequest()
	source.Client = &fosite.DefaultClient{ID: "test-client"}
	source.Session = &openid.DefaultSession{Headers: new(jwt.Headers)}

	data, err := json.Marshal(source)
	if err != nil {
		t.Fatalf("не удалось сериализовать: %v", err)
	}
	var restored fosite.Request
	if err = json.Unmarshal(data, &restored); err == nil {
		t.Fatal("ожидалась ошибка разбора: поля Client и Session — интерфейсы")
	}
}
