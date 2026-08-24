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
	"wish/services/shared/notify"

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
	// notifier доставляет коды подтверждения. Сервис оповещений может
	// быть выключен — тогда код останется недоставленным, и об этом
	// честно сообщается вызывающему.
	notifier *notify.Client
	// confirmationSecret — ключ хеширования кодов подтверждения. Тот же,
	// что подписывает токены: заводить второй секрет ради одной таблицы
	// значит удваивать то, что нужно беречь.
	confirmationSecret []byte
	// providers — внешние провайдеры входа, разобранные при старте:
	// провайдер без адреса токена или секрета не заработает, и узнать
	// об этом лучше сразу.
	providers map[string]SocialProvider
	social    *SocialClient

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
	db, err := NewDatabaseUsers(ctx, cfg)
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
		// Грант password (ROPC) не подключается намеренно: клиент получает
		// пароль пользователя в открытом виде. Его роль выполняет
		// Authorization Code Flow с PKCE.
		compose.OAuth2RefreshTokenGrantFactory,
		compose.OAuth2TokenIntrospectionFactory,
		compose.OAuth2TokenRevocationFactory,
	)
	providers, err := LoadSocialProviders(cfg.SocialProviders)
	if err != nil {
		return nil, err
	}

	return &Service{
		oauth2Config:       oauth2Config,
		oauth2Provider:     oauth2Provider,
		keyManager:         keyManager,
		db:                 db,
		cfg:                cfg,
		notifier:           notify.NewClient(cfg.NotifyEndpoint, cfg.ServiceUserId),
		confirmationSecret: secret,
		providers:          providers,
		social:             NewSocialClient(),
	}, nil
}

// CleanupExpiredTokens удаляет просроченные токены по расписанию.
func (s *Service) CleanupExpiredTokens(ctx context.Context) error {
	deleted, err := s.db.DeleteExpiredTokens(ctx)
	if err != nil {
		return err
	}
	if deleted > 0 {
		slog.Info("Expired tokens removed", slog.Int64("count", deleted))
	}
	return nil
}

// Ping нужен пробе готовности.
func (s *Service) Ping(ctx context.Context) error {
	return s.db.Ping(ctx)
}

// Stats отдаёт состояние пула соединений для метрик.
func (s *Service) Stats() services.StatsProvider {
	return s.db
}

// Close освобождает ресурсы сервиса.
func (s *Service) Close() error {
	return s.db.Close()
}

// handleCreateClient заводит OAuth2-клиента. Раньше функция существовала,
// но не была зарегистрирована ни на одном маршруте: единственным способом
// создать клиента оставался INSERT в миграции.
func (s *Service) handleCreateClient(w http.ResponseWriter, r *http.Request) {
	if !s.authorizeAdmin(r) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	clientId := r.PostFormValue("client-id")
	clientSecret := r.PostFormValue("client-secret")
	redirectUri := r.PostFormValue("redirect-uri")
	if clientId == "" || clientSecret == "" || redirectUri == "" {
		http.Error(w, "client-id, client-secret and redirect-uri are required",
			http.StatusBadRequest)
		return
	}

	scopes := r.PostFormValue("scopes")
	if scopes == "" {
		scopes = "openid,read,write"
	}
	responseTypes := r.PostFormValue("response-types")
	if responseTypes == "" {
		responseTypes = "code"
	}
	grantTypes := r.PostFormValue("grant-types")
	if grantTypes == "" {
		grantTypes = "authorization_code,refresh_token"
	}

	// Секрет хранится bcrypt-хешем: fosite сравнивает GetHashedSecret через
	// bcrypt, и открытый текст не совпал бы никогда.
	hashed, err := bcrypt.GenerateFromPassword([]byte(clientSecret), bcrypt.DefaultCost)
	if err != nil {
		slog.ErrorContext(r.Context(), "Failed to hash client secret", slog.String("err", err.Error()))
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	err = s.db.CreateClient(r.Context(), clientId, string(hashed), redirectUri, scopes, responseTypes, grantTypes)
	if s.db.IsUniqueConstraintError(err) {
		http.Error(w, "Client already exists", http.StatusConflict)
		return
	}
	if err != nil {
		slog.ErrorContext(r.Context(), "Failed to create client", slog.String("err", err.Error()))
		http.Error(w, "Can't create client", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusCreated)
}

func (s *Service) handleRegister(w http.ResponseWriter, r *http.Request) {
	username := r.PostFormValue("username")
	password := r.PostFormValue("password")
	if username == "" || password == "" {
		http.Error(w, "Username and password required", http.StatusBadRequest)
		return
	}

	// Телефон обязателен по требованию FR-02. Обязательность формы —
	// это ещё не подтверждение: пока номер не подтверждён, полагаться
	// на него нельзя, и это отражено отдельным полем.
	phone, err := NormalizePhone(r.PostFormValue("phone"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	registration := Registration{
		Username:    username,
		Phone:       phone,
		Email:       r.PostFormValue("email"),
		DisplayName: r.PostFormValue("display_name"),
		Gender:      r.PostFormValue("gender"),
	}
	if registration.Email != "" {
		if err = ValidateEmail(registration.Email); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	if registration.Gender != "" {
		if err = ValidateGender(registration.Gender); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}

	hashedPassword, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		slog.ErrorContext(r.Context(), "Failed to hashing password", slog.String("err", err.Error()))
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}
	registration.PasswordHash = string(hashedPassword)

	user, err := s.db.CreateUser(r.Context(), registration)
	if err != nil {
		if s.db.IsUniqueConstraintError(err) {
			// Что именно занято — имя, телефон или почта — наружу
			// не сообщается: это позволяло бы проверять, зарегистрирован ли
			// человек с известным номером.
			slog.WarnContext(r.Context(), "Registration conflict", slog.String("err", err.Error()))
			http.Error(w, "Username, phone or email is already taken", http.StatusConflict)
			return
		}
		slog.ErrorContext(r.Context(), "Failed to create user", slog.String("err", err.Error()))
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("X-User-Id", user.Id.String())
	w.WriteHeader(http.StatusCreated)
}

func (s *Service) handleToken(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	// Гранты authorization_code и refresh_token сами восстанавливают scope
	// из сохранённого запроса, поэтому выдавать их здесь не нужно.
	session := s.newSession(ctx, uuid.Nil)
	accessRequest, err := s.oauth2Provider.NewAccessRequest(ctx, r, session)
	if err != nil {
		slog.DebugContext(ctx, "Access request rejected", slog.String("err", err.Error()))
		s.oauth2Provider.WriteAccessError(ctx, w, accessRequest, err)
		return
	}

	if session, ok := accessRequest.GetSession().(*jwtSession); ok {
		s.applyRoles(ctx, session)
	}

	response, err := s.oauth2Provider.NewAccessResponse(ctx, accessRequest)
	if err != nil {
		slog.DebugContext(ctx, "Access response failed", slog.String("err", err.Error()))
		s.oauth2Provider.WriteAccessError(ctx, w, accessRequest, err)
		return
	}
	s.oauth2Provider.WriteAccessResponse(ctx, w, accessRequest, response)
}

// applyRoles кладёт роли пользователя в claim токена. Шлюз пробрасывает их
// в заголовок, и сервисы за ним не ходят за ролями в базу: иначе одни и те же
// данные пришлось бы держать в каждом сервисе.
func (s *Service) applyRoles(ctx context.Context, session *jwtSession) {
	subject := session.GetSubject()
	if subject == "" {
		return
	}
	userId, err := uuid.Parse(subject)
	if err != nil {
		slog.ErrorContext(ctx, "Session subject is not a uuid", slog.String("err", err.Error()))
		return
	}
	roles, err := s.db.GetUserRoles(ctx, userId)
	if err != nil {
		// Отсутствие ролей не повод не выдавать токен: пользователь просто
		// получит права по умолчанию.
		slog.ErrorContext(ctx, "Can't load user roles", slog.String("err", err.Error()))
		roles = nil
	}

	// Claim выставляется всегда, даже когда особых ролей нет. Иначе шлюзу
	// нечем перезаписать заголовок X-Roles, и пользователь без ролей
	// прислал бы себе любую роль сам. Роль RoleUser есть у каждого
	// и не даёт ничего сверх обычных прав.
	if session.JWTClaims.Extra == nil {
		session.JWTClaims.Extra = map[string]any{}
	}
	session.JWTClaims.Extra["roles"] = append([]string{services.RoleUser}, roles...)
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
			slog.DebugContext(r.Context(), "Token introspection failed", slog.String("err", err.Error()))
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
		slog.WarnContext(r.Context(), "Unauthorized key rotation attempt",
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
		slog.ErrorContext(r.Context(), "Failed to rotate keys", slog.String("err", err.Error()))
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
		slog.DebugContext(ctx, "Revocation request rejected", slog.String("err", err.Error()))
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
		slog.ErrorContext(ctx, "Can't resolve signing key id", slog.String("err", err.Error()))
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

// CleanupSocialLogins удаляет брошенные состояния внешнего входа.
func (s *Service) CleanupSocialLogins(ctx context.Context) error {
	deleted, err := s.db.DeleteExpiredSocialLogins(ctx)
	if err != nil {
		return err
	}
	if deleted > 0 {
		slog.InfoContext(ctx, "Expired social logins deleted", slog.Int64("count", deleted))
	}
	return nil
}
