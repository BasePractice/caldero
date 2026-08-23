package main

import (
	"maps"
	"slices"

	"github.com/ory/fosite"
	"github.com/ory/fosite/handler/oauth2"
	"github.com/ory/fosite/token/jwt"
)

// jwtSession — сессия для JWT-стратегии.
//
// Собственный тип нужен из-за oauth2.JWTSession.SetSubject: он заполняет
// только своё поле Subject, а сам токен собирается из JWTClaims. Password-грант
// вызывает именно SetSubject, поэтому claim sub уходил пустым — и шлюзу
// нечего было пробрасывать в X-Authorized-Id, то есть вся цепочка
// аутентификации обрывалась на выданном токене.
type jwtSession struct {
	oauth2.JWTSession
}

func (s *jwtSession) SetSubject(subject string) {
	s.Subject = subject
	s.GetJWTClaims()
	s.JWTClaims.Subject = subject
}

// Clone копируется вручную: реализация в fosite вернула бы встроенный
// oauth2.JWTSession, и SetSubject снова перестал бы доходить до claims.
func (s *jwtSession) Clone() fosite.Session {
	if s == nil {
		return nil
	}
	clone := &jwtSession{}
	clone.Username = s.Username
	clone.Subject = s.Subject

	if s.JWTClaims != nil {
		claims := *s.JWTClaims
		claims.Audience = slices.Clone(s.JWTClaims.Audience)
		claims.Scope = slices.Clone(s.JWTClaims.Scope)
		claims.Extra = maps.Clone(s.JWTClaims.Extra)
		clone.JWTClaims = &claims
	}
	if s.JWTHeader != nil {
		clone.JWTHeader = &jwt.Headers{Extra: maps.Clone(s.JWTHeader.Extra)}
	}
	if s.ExpiresAt != nil {
		clone.ExpiresAt = maps.Clone(s.ExpiresAt)
	}
	return clone
}
