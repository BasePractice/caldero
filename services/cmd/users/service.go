package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"wish/services"

	"github.com/go-jose/go-jose/v4"
	"github.com/google/uuid"
	"github.com/ory/fosite"
	"github.com/ory/fosite/compose"
	"github.com/ory/fosite/handler/oauth2"
	"github.com/ory/fosite/token/jwt"
	"golang.org/x/crypto/bcrypt"
)

type Service struct {
	oauth2Config   *fosite.Config
	oauth2Provider fosite.OAuth2Provider
	keyManager     KeyManager
	db             DatabaseUsers
	cfg            services.Config

	rotationMu   sync.Mutex
	lastRotation time.Time
}

// oauth2Secret возвращает секрет подписи. Раньше он генерировался случайно
// при каждом старте: токены не переживали рестарт, а два инстанса не могли
// проверить токены друг друга.
func oauth2Secret(cfg services.Config) ([]byte, error) {
	const secretLen = 32

	if cfg.OAuth2GlobalSecret == "" {
		secret := make([]byte, secretLen)
		if _, err := rand.Read(secret); err != nil {
			return nil, fmt.Errorf("generating fallback oauth2 secret: %w", err)
		}
		slog.Warn("OAUTH2_GLOBAL_SECRET is not set, using a random secret: " +
			"issued tokens will not survive a restart and other instances will reject them")
		return secret, nil
	}

	secret := []byte(cfg.OAuth2GlobalSecret)
	if len(secret) != secretLen {
		return nil, fmt.Errorf("OAUTH2_GLOBAL_SECRET must be exactly %d bytes, got %d",
			secretLen, len(secret))
	}
	return secret, nil
}

func newService(ctx context.Context, cfg services.Config) (*Service, error) {
	secret, err := oauth2Secret(cfg)
	if err != nil {
		return nil, err
	}

	var oauth2Config = &fosite.Config{
		AccessTokenLifespan:        time.Hour,
		RefreshTokenLifespan:       time.Hour * 24 * 30,
		IDTokenLifespan:            time.Hour,
		SendDebugMessagesToClients: cfg.OAuth2Debug,
		GlobalSecret:               secret,
		AuthorizeCodeLifespan:      10 * time.Minute,
		// Публичный клиент не может сохранить секрет, поэтому без PKCE
		// перехваченный код обменивается на токен кем угодно.
		EnforcePKCEForPublicClients: true,
	}
	db, err := NewDatabaseUsers(cfg)
	if err != nil {
		return nil, err
	}
	keyManager, err := NewKeyManager(ctx, db)
	if err != nil {
		return nil, fmt.Errorf("creating key manager: %w", err)
	}
	var oauth2Provider = compose.Compose(
		oauth2Config,
		db,
		&compose.CommonStrategy{
			// Access-токен подписывается RS256 поверх HMAC-стратегии: шлюз
			// проверяет подпись по JWKS, а непрозрачную HMAC-строку
			// проверить нечем.
			CoreStrategy:               compose.NewOAuth2JWTStrategy(keyManager.GetPrivateKey, compose.NewOAuth2HMACStrategy(oauth2Config), oauth2Config),
			OpenIDConnectTokenStrategy: compose.NewOpenIDConnectStrategy(keyManager.GetPrivateKey, oauth2Config),
			Signer:                     &jwt.DefaultSigner{GetPrivateKey: keyManager.GetPrivateKey},
		},
		compose.OAuth2AuthorizeExplicitFactory,
		compose.OAuth2PKCEFactory,
		compose.OAuth2ResourceOwnerPasswordCredentialsFactory,
		compose.OAuth2RefreshTokenGrantFactory,
		compose.OAuth2TokenIntrospectionFactory,
		compose.OAuth2TokenRevocationFactory,
	)
	return &Service{
		oauth2Config:   oauth2Config,
		oauth2Provider: oauth2Provider,
		keyManager:     keyManager,
		db:             db,
		cfg:            cfg,
	}, nil
}

// Close освобождает ресурсы сервиса.
func (s *Service) Close() error {
	return s.db.Close()
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
		slog.Error("Failed to hashing password", slog.String("err", err.Error()))
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	user, err := s.db.CreateUser(r.Context(), username, string(hashedPassword))
	if err != nil {
		if s.db.IsUniqueConstraintError(err) {
			slog.Error("User already exists", slog.String("username", username), slog.String("err", err.Error()))
			http.Error(w, "Username already exists", http.StatusConflict)
			return
		}
		slog.Error("Failed to create user", slog.String("err", err.Error()))
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("X-User-Id", user.Id.String())
	w.WriteHeader(http.StatusCreated)
}

func (s *Service) handleToken(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	// Пользователя аутентифицирует сам password-грант через
	// DatabaseUsers.Authenticate. Раньше это делалось вручную после разбора
	// запроса, причём фабрика гранта вообще не была зарегистрирована,
	// и эндпоинт не поддерживал ни одного гранта, кроме refresh_token.
	session := s.newSession(ctx, uuid.Nil)
	accessRequest, err := s.oauth2Provider.NewAccessRequest(ctx, r, session)
	if err != nil {
		slog.Debug("Access request rejected", slog.String("err", err.Error()))
		s.oauth2Provider.WriteAccessError(ctx, w, accessRequest, err)
		return
	}

	// fosite проверяет запрошенные scope против клиента, но не выдаёт их:
	// решение, какие scope выдать, остаётся за сервером авторизации.
	// У остальных грантов это делают их собственные обработчики — refresh
	// восстанавливает исходный набор, authorization_code берёт его из
	// сохранённого запроса, — а password-грант оставлял granted scope пустым.
	if accessRequest.GetGrantTypes().ExactOne("password") {
		for _, scope := range accessRequest.GetRequestedScopes() {
			accessRequest.GrantScope(scope)
		}
		for _, audience := range accessRequest.GetRequestedAudience() {
			accessRequest.GrantAudience(audience)
		}
	}

	response, err := s.oauth2Provider.NewAccessResponse(ctx, accessRequest)
	if err != nil {
		slog.Debug("Access response failed", slog.String("err", err.Error()))
		s.oauth2Provider.WriteAccessError(ctx, w, accessRequest, err)
		return
	}
	s.oauth2Provider.WriteAccessResponse(ctx, w, accessRequest, response)
}

// requesterKey — приватный тип ключа контекста. Строковый ключ "claims"
// мог столкнуться с ключом любого другого пакета (staticcheck SA1029).
type requesterKey struct{}

func requesterFromContext(ctx context.Context) (fosite.Requester, bool) {
	requester, ok := ctx.Value(requesterKey{}).(fosite.Requester)
	return requester, ok
}

// protect проверяет access-токен через introspection. Раньше тот же токен
// дополнительно скармливался jwt.ParseWithClaims, хотя CoreStrategy — HMAC,
// и access-токен не является JWT: разбор не мог завершиться успешно никогда,
// а результат introspection отбрасывался.
func (s *Service) protect(protected http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := fosite.AccessTokenFromRequest(r)
		_, requester, err := s.oauth2Provider.IntrospectToken(
			r.Context(), token, fosite.AccessToken, s.newSession(r.Context(), uuid.Nil))
		if err != nil {
			slog.Debug("Token introspection failed", slog.String("err", err.Error()))
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		protected(w, r.WithContext(context.WithValue(r.Context(), requesterKey{}, requester)))
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

// authorizeAdmin защищает служебные эндпоинты. Сравнение постоянного времени:
// обычное сравнение строк утекает длину совпадающего префикса.
func (s *Service) authorizeAdmin(r *http.Request) bool {
	if s.cfg.AdminToken == "" {
		return false
	}
	provided := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	return subtle.ConstantTimeCompare([]byte(provided), []byte(s.cfg.AdminToken)) == 1
}

func (s *Service) handleRotateKeys(w http.ResponseWriter, r *http.Request) {
	if !s.authorizeAdmin(r) {
		slog.Warn("Unauthorized key rotation attempt",
			slog.String("remote", r.RemoteAddr))
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	// Ротация генерирует RSA-2048 и обесценивает выданные токены, поэтому
	// её частота ограничена даже для владельца административного токена.
	s.rotationMu.Lock()
	defer s.rotationMu.Unlock()
	if since := time.Since(s.lastRotation); !s.lastRotation.IsZero() && since < s.cfg.KeyRotationMinInterval {
		w.Header().Set("Retry-After",
			strconv.Itoa(int((s.cfg.KeyRotationMinInterval - since).Seconds())))
		http.Error(w, "Key rotation is rate limited", http.StatusTooManyRequests)
		return
	}

	if err := s.keyManager.RotateKeys(r.Context()); err != nil {
		slog.Error("Failed to rotate keys", slog.String("err", err.Error()))
		http.Error(w, "Can't rotate keys", http.StatusInternalServerError)
		return
	}
	s.lastRotation = time.Now()
	w.WriteHeader(http.StatusOK)
}

// handleRevoke реализует RFC 7009: аутентификацию клиента и подбор корректного
// ответа делает fosite. Раньше наружу уходил err.Error() с внутренними деталями,
// а неизвестный токен давал 500 вместо предписанного стандартом 200.
func (s *Service) handleRevoke(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	err := s.oauth2Provider.NewRevocationRequest(ctx, r)
	if err != nil {
		slog.Debug("Revocation request rejected", slog.String("err", err.Error()))
	}
	s.oauth2Provider.WriteRevocationResponse(ctx, w, err)
}

// newSession возвращает oauth2.JWTSession: JWT-стратегия требует именно
// контейнер JWT-claims, с openid.DefaultSession выдача токена падает
// с "Session must be of type JWTSessionContainer".
func (s *Service) newSession(ctx context.Context, userId uuid.UUID) *jwtSession {
	headers := jwt.NewHeaders()
	// Без kid проверяющая сторона не знает, каким ключом из JWKS проверять
	// подпись, и отвергает корректный токен.
	if kid, err := s.keyManager.GetPublicKeyId(ctx); err != nil {
		slog.Error("Can't resolve signing key id", slog.String("err", err.Error()))
	} else if kid != "" {
		headers.Add("kid", kid)
	}

	subject := ""
	if userId != uuid.Nil {
		subject = userId.String()
	}
	return &jwtSession{oauth2.JWTSession{
		JWTClaims: &jwt.JWTClaims{
			Subject:   subject,
			Issuer:    s.cfg.OAuth2Issuer,
			IssuedAt:  time.Now(),
			ExpiresAt: time.Now().Add(s.oauth2Config.AccessTokenLifespan),
		},
		JWTHeader: headers,
		Subject:   subject,
	}}
}
