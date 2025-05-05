package main

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/google/uuid"
	"github.com/ory/fosite"
	"github.com/ory/fosite/compose"
	"github.com/ory/fosite/handler/openid"
	"github.com/ory/fosite/token/jwt"
	"golang.org/x/crypto/bcrypt"
)

type Service struct {
	oauth2Config   *fosite.Config
	oauth2Provider fosite.OAuth2Provider
	keyManager     KeyManager
	db             DatabaseUsers
}

func newService(ctx context.Context) *Service {
	var oauth2Config = &fosite.Config{
		AccessTokenLifespan:        time.Hour,
		RefreshTokenLifespan:       time.Hour * 24 * 30,
		IDTokenLifespan:            time.Hour,
		SendDebugMessagesToClients: true,
	}
	db := NewDatabaseUsers()
	keyManager, _ := NewKeyManager(ctx, db)
	var oauth2Provider = compose.Compose(
		oauth2Config,
		db,
		keyManager,
		compose.OAuth2AuthorizeExplicitFactory,
		compose.OAuth2RefreshTokenGrantFactory,
		compose.OpenIDConnectExplicitFactory,
		compose.OAuth2TokenIntrospectionFactory,
		compose.OAuth2TokenRevocationFactory,
	)
	return &Service{
		oauth2Config:   oauth2Config,
		oauth2Provider: oauth2Provider,
		keyManager:     keyManager,
		db:             db,
	}
}

func (s *Service) handleCreateClient(w http.ResponseWriter, r *http.Request) {
	clientId := r.FormValue("client-id")
	clientSecret := r.FormValue("client-secret")
	redirectUri := r.FormValue("redirect-uri")
	scopes := r.FormValue("scopes")
	if scopes == "" {
		scopes = "openid,read,write"
	}
	responseTypes := r.FormValue("response-types")
	if responseTypes == "" {
		responseTypes = "code"
	}
	grantTypes := r.FormValue("grant-types")
	if grantTypes == "" {
		grantTypes = "authorization_code,refresh_token,password"
	}
	s.db.CreateClient(r.Context(), clientId, clientSecret, redirectUri, scopes, responseTypes, grantTypes)
}

func (s *Service) handleRegister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	username := r.FormValue("username")
	password := r.FormValue("password")

	if username == "" || password == "" {
		http.Error(w, "Username and password required", http.StatusBadRequest)
		return
	}

	hashedPassword, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	user, err := s.db.CreateUser(r.Context(), username, string(hashedPassword))
	if err != nil {
		if s.db.IsUniqueConstraintError(err) {
			http.Error(w, "Username already exists", http.StatusConflict)
			return
		}
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusCreated)
	w.Header().Set("X-User-Id", user.Id.String())
}

func (s *Service) handleAuthorization(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ar, err := s.oauth2Provider.NewAuthorizeRequest(ctx, r)
	if err != nil {
		s.oauth2Provider.WriteAuthorizeError(ctx, w, ar, err)
		return
	}

	user, err := s.authenticateUser(r)
	if err != nil {
		http.Error(w, "Invalid credentials", http.StatusUnauthorized)
		return
	}

	session := s.newSession(user.Id)
	response, err := s.oauth2Provider.NewAuthorizeResponse(ctx, ar, session)
	if err != nil {
		s.oauth2Provider.WriteAuthorizeError(ctx, w, ar, err)
		return
	}

	s.oauth2Provider.WriteAuthorizeResponse(ctx, w, ar, response)
}

func (s *Service) handleToken(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	session := s.newSession(uuid.Nil)
	accessRequest, err := s.oauth2Provider.NewAccessRequest(ctx, r, session)
	if err != nil {
		s.oauth2Provider.WriteAccessError(ctx, w, accessRequest, err)
		return
	}

	// Для grant_type=password аутентифицируем пользователя
	if accessRequest.GetGrantTypes().ExactOne("password") {
		user, err := s.authenticateUser(r)
		if err != nil {
			s.oauth2Provider.WriteAccessError(ctx, w, accessRequest, err)
			return
		}
		session.Subject = user.Id.String()
	}

	response, err := s.oauth2Provider.NewAccessResponse(ctx, accessRequest)
	if err != nil {
		s.oauth2Provider.WriteAccessError(ctx, w, accessRequest, err)
		return
	}

	s.oauth2Provider.WriteAccessResponse(ctx, w, accessRequest, response)
}

func (s *Service) protect(protected func(w http.ResponseWriter, r *http.Request)) func(w http.ResponseWriter, r *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		token := fosite.AccessTokenFromRequest(r)

		_, _, err := s.oauth2Provider.IntrospectToken(ctx, token, fosite.AccessToken, s.newSession(uuid.Nil))
		if err != nil {
			http.Error(w, err.Error(), http.StatusUnauthorized)
			return
		}
		jwtToken, err := jwt.ParseWithClaims(token, jwt.MapClaims{}, func(token *jwt.Token) (interface{}, error) {
			kid, ok := token.Header["kid"].(string)
			if !ok {
				return nil, errors.New("kid header missing")
			}
			return s.keyManager.GetPublicKey(r.Context(), kid)
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusUnauthorized)
			return
		}
		claims := jwtToken.Claims
		protected(w, r.WithContext(context.WithValue(r.Context(), "claims", claims)))
	}
}

func (s *Service) handleJWKS(w http.ResponseWriter, r *http.Request) {
	var keys []jose.JSONWebKey
	pks, err := s.keyManager.GetKeys(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	for _, key := range pks {
		pk, err := x509.ParsePKCS1PrivateKey(key.Data)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		keys = append(keys, jose.JSONWebKey{
			KeyID:     key.Id,
			Algorithm: "RS256",
			Use:       "sig",
			Key:       &pk.PublicKey,
		})
	}
	w.Header().Set("Content-Type", "application/json")
	err = json.NewEncoder(w).Encode(map[string]interface{}{"keys": keys})
	if err != nil {
		slog.Error("Error encoding keys", slog.String("error", err.Error()))
	}
}

func (s *Service) handleRotateKeys(w http.ResponseWriter, r *http.Request) {
	if err := s.keyManager.RotateKeys(r.Context()); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Service) handleRevoke(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	err := s.oauth2Provider.NewRevocationRequest(ctx, r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Service) newSession(userId uuid.UUID) *openid.DefaultSession {
	return &openid.DefaultSession{
		Claims: &jwt.IDTokenClaims{
			Subject:   userId.String(),
			ExpiresAt: time.Now().Add(s.oauth2Config.IDTokenLifespan),
		},
		Headers: new(jwt.Headers),
	}
}

func (s *Service) authenticateUser(r *http.Request) (*User, error) {
	username := r.FormValue("username")
	password := r.FormValue("password")

	user, err := s.db.GetUser(r.Context(), username, password)
	if err != nil {
		return nil, err
	}
	if err = bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(password)); err != nil {
		return nil, err
	}

	return user, nil
}
