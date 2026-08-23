package main

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"wish/services"

	"github.com/google/uuid"
	_ "github.com/lib/pq"
	"github.com/ory/fosite"
	"github.com/ory/fosite/handler/oauth2"
	"golang.org/x/crypto/bcrypt"
)

//go:embed migrations/*.sql
var migrations embed.FS

type DatabaseUsers interface {
	oauth2.CoreStorage
	GetClient(ctx context.Context, id string) (fosite.Client, error)
	CreateAccessTokenSession(ctx context.Context, signature string, request fosite.Requester) error
	GetAccessTokenSession(ctx context.Context, signature string, session fosite.Session) (fosite.Requester, error)
	DeleteAccessTokenSession(ctx context.Context, signature string) error
	GetRefreshTokenSession(ctx context.Context, signature string, session fosite.Session) (fosite.Requester, error)
	DeleteRefreshTokenSession(ctx context.Context, signature string) error
	RevokeRefreshToken(ctx context.Context, requestID string) error
	RevokeAccessToken(ctx context.Context, requestID string) error

	CreatePKCERequestSession(ctx context.Context, signature string, requester fosite.Requester) error
	GetPKCERequestSession(ctx context.Context, signature string, session fosite.Session) (fosite.Requester, error)
	DeletePKCERequestSession(ctx context.Context, signature string) error

	CreateUser(ctx context.Context, registration Registration) (*User, error)
	GetUser(ctx context.Context, username string) (*User, error)
	GetUserById(ctx context.Context, id uuid.UUID) (*User, error)
	UpdateProfile(ctx context.Context, id uuid.UUID, update ProfileUpdate) (*User, error)
	Authenticate(ctx context.Context, username, secret string) (string, error)

	// Подтверждение телефона и почты.
	CreateConfirmation(ctx context.Context, confirmation Confirmation) (Confirmation, error)
	// ActiveConfirmation отдаёт последний код, который ещё можно предъявить.
	ActiveConfirmation(ctx context.Context, user uuid.UUID, kind ConfirmationKind) (Confirmation, error)
	// CountConfirmations считает выданные коды за окно: без этого
	// эндпоинт отправки превращается в средство рассылки за чужой счёт.
	CountConfirmations(ctx context.Context, user uuid.UUID, kind ConfirmationKind, window time.Duration) (int, time.Time, error)
	// FailConfirmation засчитывает неудачную попытку.
	FailConfirmation(ctx context.Context, id uuid.UUID) error
	// ConfirmContact отмечает контакт подтверждённым.
	ConfirmContact(ctx context.Context, id uuid.UUID, user uuid.UUID, kind ConfirmationKind, target string) error

	// Вход через внешних провайдеров.
	StartSocialLogin(ctx context.Context, login SocialLogin) error
	TakeSocialLogin(ctx context.Context, state string) (SocialLogin, error)
	DeleteExpiredSocialLogins(ctx context.Context) (int64, error)
	IdentityUser(ctx context.Context, provider, externalId string) (uuid.UUID, error)
	LinkIdentity(ctx context.Context, user uuid.UUID, profile SocialProfile) error
	Identities(ctx context.Context, user uuid.UUID) ([]Identity, error)
	UnlinkIdentity(ctx context.Context, user uuid.UUID, provider string) error
	CreateSocialUser(ctx context.Context, profile SocialProfile) (*User, error)
	GetUserRoles(ctx context.Context, userId uuid.UUID) ([]string, error)
	DeleteExpiredTokens(ctx context.Context) (int64, error)

	GetLastKey(ctx context.Context) (string, error)
	GetKey(ctx context.Context, id string) ([]byte, error)
	GetKeys(ctx context.Context, cb func(string, []byte)) error
	CreateKey(ctx context.Context, key []byte) (string, error)
	ClearKeys(ctx context.Context) error

	IsUniqueConstraintError(err error) bool
	CreateClient(ctx context.Context, clientId, clientSecret, redirectUri, scopes, responseType, grantTypes string) error
	// Close освобождает соединения с БД
	Close() error
	// Stats нужен для публикации метрик пула соединений.
	Stats() sql.DBStats
	// Ping нужен пробе готовности.
	Ping(ctx context.Context) error
}

type ds struct {
	db *sql.DB
	// cipher шифрует приватные ключи подписи. nil означает хранение
	// открытым текстом — только для локального стенда.
	cipher *services.Cipher
}

func (s *ds) CreateAuthorizeCodeSession(ctx context.Context, code string, request fosite.Requester) error {
	return s.createTokenSession(ctx, "code", code, request)
}

// GetAuthorizeCodeSession отличает использованный код от несуществующего:
// fosite ожидает ErrInvalidatedAuthorizeCode вместе с запросом, чтобы отозвать
// все токены, выданные по этому запросу. Молчаливое "не найдено" оставило бы
// перехват кода незамеченным.
func (s *ds) GetAuthorizeCodeSession(ctx context.Context, code string, session fosite.Session) (fosite.Requester, error) {
	request, used, err := s.getCodeSession(ctx, code, session)
	if err != nil {
		return nil, err
	}
	if used {
		return request, fosite.ErrInvalidatedAuthorizeCode
	}
	return request, nil
}

func (s *ds) InvalidateAuthorizeCodeSession(ctx context.Context, code string) error {
	result, err := s.db.ExecContext(ctx,
		"UPDATE oauth_tokens SET used = TRUE WHERE signature = $1 AND token_type = 'code'", code)
	if err != nil {
		return fmt.Errorf("invalidating authorize code: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("invalidating authorize code: %w", err)
	}
	if affected == 0 {
		return fosite.ErrNotFound
	}
	return nil
}

func (s *ds) CreatePKCERequestSession(ctx context.Context, signature string, request fosite.Requester) error {
	return s.createTokenSession(ctx, "pkce", signature, request)
}

func (s *ds) GetPKCERequestSession(ctx context.Context, signature string, session fosite.Session) (fosite.Requester, error) {
	return s.getTokenSession(ctx, "pkce", signature, session)
}

func (s *ds) DeletePKCERequestSession(ctx context.Context, signature string) error {
	return s.deleteTokenSession(ctx, "pkce", signature)
}

// RotateRefreshToken вызывается refresh-потоком безусловно, поэтому паника
// здесь ломала обновление токена. Поведение как в референсной реализации
// fosite: старая пара отзывается целиком.
func (s *ds) RotateRefreshToken(ctx context.Context, requestID string, _ string) error {
	if err := s.RevokeRefreshToken(ctx, requestID); err != nil {
		return fmt.Errorf("revoking refresh token of request %s: %w", requestID, err)
	}
	if err := s.RevokeAccessToken(ctx, requestID); err != nil {
		return fmt.Errorf("revoking access token of request %s: %w", requestID, err)
	}
	return nil
}

func (s *ds) CreateClient(ctx context.Context, clientId, clientSecret, redirectUri, scopes, responseType, grantTypes string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO oauth_clients (client_id, client_secret, redirect_uris, grant_types, response_types, scopes)
		 VALUES ($1, $2, $3, $4, $5, $6)`,
		clientId, clientSecret, redirectUri, grantTypes, responseType, scopes)
	if err != nil {
		return fmt.Errorf("creating client %s: %w", clientId, err)
	}
	return nil
}

func (s *ds) GetKeys(ctx context.Context, cb func(string, []byte)) error {
	rows, err := s.db.QueryContext(ctx,
		"SELECT key_id, private_key, encrypted FROM keys ORDER BY created_at DESC LIMIT 2")
	if err != nil {
		return fmt.Errorf("loading signing keys: %w", err)
	}
	defer func() {
		// Настоящая причина сбоя придёт из rows.Err().
		_ = rows.Close()
	}()

	for rows.Next() {
		var id string
		var privateKey []byte
		var encrypted bool
		if err = rows.Scan(&id, &privateKey, &encrypted); err != nil {
			return fmt.Errorf("scanning signing key: %w", err)
		}
		decoded, err := s.decodeKey(privateKey, encrypted)
		if err != nil {
			return err
		}
		cb(id, decoded)
	}
	// Без этой проверки обрыв соединения посреди выборки выглядит как пустой
	// набор ключей, и JWKS молча отдаёт пустой список.
	if err = rows.Err(); err != nil {
		return fmt.Errorf("reading signing keys: %w", err)
	}
	return nil
}

func (s *ds) GetKey(ctx context.Context, id string) ([]byte, error) {
	var privateKey []byte
	var encrypted bool
	err := s.db.QueryRowContext(ctx,
		"SELECT private_key, encrypted FROM keys WHERE key_id = $1", id).
		Scan(&privateKey, &encrypted)
	if err != nil {
		return nil, fmt.Errorf("loading signing key %s: %w", id, err)
	}
	return s.decodeKey(privateKey, encrypted)
}

func (s *ds) CreateKey(ctx context.Context, key []byte) (string, error) {
	keyId := fmt.Sprintf("key-%d", time.Now().UnixNano())

	stored, encrypted := key, false
	if s.cipher != nil {
		var err error
		if stored, err = s.cipher.Encrypt(key); err != nil {
			return "", fmt.Errorf("encrypting signing key: %w", err)
		}
		encrypted = true
	}

	_, err := s.db.ExecContext(ctx,
		"INSERT INTO keys (key_id, private_key, encrypted) VALUES ($1, $2, $3)",
		keyId, stored, encrypted)
	if err != nil {
		return "", fmt.Errorf("storing signing key: %w", err)
	}
	return keyId, nil
}

// decodeKey возвращает ключ в открытом виде. Признак хранится рядом с самой
// записью, поэтому включение шифрования не ломает ранее выданные ключи.
func (s *ds) decodeKey(stored []byte, encrypted bool) ([]byte, error) {
	if !encrypted {
		return stored, nil
	}
	if s.cipher == nil {
		return nil, fmt.Errorf("signing key is encrypted but KEY_MASTER_KEY is not set")
	}
	key, err := s.cipher.Decrypt(stored)
	if err != nil {
		return nil, fmt.Errorf("decrypting signing key: %w", err)
	}
	return key, nil
}

func (s *ds) ClearKeys(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM keys
		WHERE key_id NOT IN (
			SELECT key_id
			FROM keys
			ORDER BY created_at DESC
			LIMIT 2
		)`)
	return err
}

func (s *ds) GetLastKey(ctx context.Context) (string, error) {
	var keyId string
	err := s.db.QueryRowContext(ctx,
		"SELECT key_id FROM keys ORDER BY created_at DESC LIMIT 1",
	).Scan(&keyId)
	if err != nil {
		return "", err
	}
	return keyId, nil
}

const userColumns = `user_id, username, password_hash, display_name, email,
	phone, phone_confirmed, email_confirmed, gender, password_set, created_at`

func scanUser(row interface{ Scan(...any) error }) (*User, error) {
	var u User
	err := row.Scan(&u.Id, &u.Username, &u.PasswordHash, &u.DisplayName, &u.Email,
		&u.Phone, &u.PhoneConfirmed, &u.EmailConfirmed, &u.Gender,
		&u.PasswordSet, &u.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &u, nil
}

func (s *ds) GetUser(ctx context.Context, username string) (*User, error) {
	return scanUser(s.db.QueryRowContext(ctx,
		"SELECT "+userColumns+" FROM users WHERE username = $1", username))
}

func (s *ds) GetUserById(ctx context.Context, id uuid.UUID) (*User, error) {
	return scanUser(s.db.QueryRowContext(ctx,
		"SELECT "+userColumns+" FROM users WHERE user_id = $1", id))
}

// UpdateProfile меняет только переданные поля: nil означает «не трогать»,
// а пустая строка — «очистить». Без этого различия любое обновление
// затирало бы поля, которых клиент не касался.
func (s *ds) UpdateProfile(ctx context.Context, id uuid.UUID, update ProfileUpdate) (*User, error) {
	return scanUser(s.db.QueryRowContext(ctx, `
		UPDATE users SET
			display_name = COALESCE($2, display_name),
			email        = COALESCE($3, email),
			phone        = COALESCE($4, phone),
			gender       = COALESCE($5, gender),
			-- Смена телефона сбрасывает подтверждение: подтверждён был
			-- прежний номер, а не новый.
			phone_confirmed = CASE WHEN $4 IS NOT NULL AND $4 IS DISTINCT FROM phone
			                       THEN FALSE ELSE phone_confirmed END,
			-- То же с почтой: подтверждён был прежний адрес.
			email_confirmed = CASE WHEN $3 IS NOT NULL AND $3 IS DISTINCT FROM email
			                       THEN FALSE ELSE email_confirmed END,
			updated_at   = now()
		WHERE user_id = $1
		RETURNING `+userColumns,
		id, nullable(update.DisplayName), nullable(update.Email),
		nullable(update.Phone), nullable(update.Gender)))
}

// nullable превращает пустую строку в NULL: клиент очищает поле, передавая
// пустое значение.
func nullable(value *string) any {
	if value == nil {
		return nil
	}
	if *value == "" {
		return nil
	}
	return *value
}

// Authenticate проверяет учётные данные на форме входа. Неизвестный
// пользователь и неверный пароль дают один и тот же ответ: различие
// позволило бы перебирать логины.
func (s *ds) Authenticate(ctx context.Context, username, secret string) (string, error) {
	user, err := s.GetUser(ctx, username)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fosite.ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("loading user %q: %w", username, err)
	}
	if err = bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(secret)); err != nil {
		return "", fosite.ErrNotFound
	}
	return user.Id.String(), nil
}

// DeleteExpiredTokens убирает просроченные записи: таблица росла бесконечно,
// удаления не было нигде. Использованные коды авторизации удаляются вместе
// с остальными — их срок жизни короткий, и после истечения они бесполезны
// даже для обнаружения повторного предъявления.
func (s *ds) DeleteExpiredTokens(ctx context.Context) (int64, error) {
	result, err := s.db.ExecContext(ctx, "DELETE FROM oauth_tokens WHERE expires_at < now()")
	if err != nil {
		return 0, fmt.Errorf("deleting expired tokens: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("counting deleted tokens: %w", err)
	}
	return affected, nil
}

func (s *ds) GetUserRoles(ctx context.Context, userId uuid.UUID) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT role FROM user_roles WHERE user_id = $1 ORDER BY role", userId)
	if err != nil {
		return nil, fmt.Errorf("loading roles of user %s: %w", userId, err)
	}
	defer func() {
		// Настоящая причина сбоя придёт из rows.Err().
		_ = rows.Close()
	}()

	var roles []string
	for rows.Next() {
		var role string
		if err = rows.Scan(&role); err != nil {
			return nil, fmt.Errorf("scanning role of user %s: %w", userId, err)
		}
		roles = append(roles, role)
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("reading roles of user %s: %w", userId, err)
	}
	return roles, nil
}

// Registration — данные для создания пользователя.
type Registration struct {
	Username     string
	PasswordHash string
	Phone        string
	Email        string
	DisplayName  string
	Gender       string
}

func (s *ds) CreateUser(ctx context.Context, registration Registration) (*User, error) {
	return scanUser(s.db.QueryRowContext(ctx, `
		INSERT INTO users (username, password_hash, phone, email, display_name, gender)
		VALUES ($1, $2, NULLIF($3, ''), NULLIF($4, ''), NULLIF($5, ''), NULLIF($6, ''))
		RETURNING `+userColumns,
		registration.Username, registration.PasswordHash, registration.Phone,
		registration.Email, registration.DisplayName, registration.Gender))
}

func (s *ds) GetClient(ctx context.Context, id string) (fosite.Client, error) {
	var client fosite.DefaultClient
	var redirectURIs, grantTypes, responseTypes, scopes string

	err := s.db.QueryRowContext(ctx,
		`SELECT client_id, client_secret, redirect_uris, 
		grant_types, response_types, scopes 
		FROM oauth_clients 
		WHERE client_id = $1`,
		id,
	).Scan(
		&client.ID,
		&client.Secret,
		&redirectURIs,
		&grantTypes,
		&responseTypes,
		&scopes,
	)
	if err != nil {
		return nil, err
	}

	client.RedirectURIs = splitCSV(redirectURIs)
	client.GrantTypes = splitCSV(grantTypes)
	client.ResponseTypes = splitCSV(responseTypes)
	client.Scopes = splitCSV(scopes)
	// Клиент без секрета — публичный: браузерное приложение не может
	// сохранить секрет, и притворяться, что может, значит выдавать
	// перехваченный код за доказательство подлинности. Для таких клиентов
	// обязателен PKCE (EnforcePKCEForPublicClients).
	client.Public = len(client.Secret) == 0

	return &client, nil
}

func (s *ds) CreateAccessTokenSession(ctx context.Context, signature string, request fosite.Requester) error {
	return s.createTokenSession(ctx, "access", signature, request)
}

func (s *ds) GetAccessTokenSession(ctx context.Context, signature string, session fosite.Session) (fosite.Requester, error) {
	return s.getTokenSession(ctx, "access", signature, session)
}

func (s *ds) DeleteAccessTokenSession(ctx context.Context, signature string) error {
	return s.deleteTokenSession(ctx, "access", signature)
}

func (s *ds) CreateRefreshTokenSession(ctx context.Context, signature string, accessSignature string, request fosite.Requester) (err error) {
	return s.createTokenSession(ctx, "refresh", signature, request)
}

func (s *ds) GetRefreshTokenSession(ctx context.Context, signature string, session fosite.Session) (fosite.Requester, error) {
	return s.getTokenSession(ctx, "refresh", signature, session)
}

func (s *ds) DeleteRefreshTokenSession(ctx context.Context, signature string) error {
	return s.deleteTokenSession(ctx, "refresh", signature)
}

// tokenSession — то, что действительно кладётся в БД. Сериализовать
// fosite.Request целиком нельзя: поля Client и Session объявлены
// интерфейсами, и json.Unmarshal падает с
// "cannot unmarshal object into Go struct field Request.client".
// Клиент восстанавливается по идентификатору, сессия раскладывается
// в переданный конкретный тип.
type tokenSession struct {
	RequestID         string          `json:"request_id"`
	ClientID          string          `json:"client_id"`
	RequestedAt       time.Time       `json:"requested_at"`
	RequestedScope    []string        `json:"requested_scope"`
	GrantedScope      []string        `json:"granted_scope"`
	RequestedAudience []string        `json:"requested_audience"`
	GrantedAudience   []string        `json:"granted_audience"`
	Form              url.Values      `json:"form"`
	Session           json.RawMessage `json:"session"`
}

// encodeTokenSession и decodeTokenSession вынесены отдельно от работы с БД:
// именно здесь была ошибка, и проверить их можно без поднятой базы.
func encodeTokenSession(request fosite.Requester) ([]byte, error) {
	sessionData, err := json.Marshal(request.GetSession())
	if err != nil {
		return nil, fmt.Errorf("marshalling session: %w", err)
	}
	data, err := json.Marshal(tokenSession{
		RequestID:         request.GetID(),
		ClientID:          request.GetClient().GetID(),
		RequestedAt:       request.GetRequestedAt(),
		RequestedScope:    request.GetRequestedScopes(),
		GrantedScope:      request.GetGrantedScopes(),
		RequestedAudience: request.GetRequestedAudience(),
		GrantedAudience:   request.GetGrantedAudience(),
		Form:              request.GetRequestForm(),
		Session:           sessionData,
	})
	if err != nil {
		return nil, fmt.Errorf("marshalling token session: %w", err)
	}
	return data, nil
}

// decodeTokenSession восстанавливает запрос. Сессия раскладывается
// в переданный экземпляр: он приходит конкретным типом, тогда как поле
// Session в fosite.Request — интерфейс, и разложить в него JSON нельзя.
func decodeTokenSession(data []byte, session fosite.Session, client fosite.Client) (*fosite.Request, string, error) {
	var stored tokenSession
	if err := json.Unmarshal(data, &stored); err != nil {
		return nil, "", fmt.Errorf("unmarshalling token session: %w", err)
	}
	if client == nil {
		return nil, stored.ClientID, nil
	}
	if session != nil && len(stored.Session) > 0 {
		if err := json.Unmarshal(stored.Session, session); err != nil {
			return nil, stored.ClientID, fmt.Errorf("unmarshalling session: %w", err)
		}
	}

	request := fosite.NewRequest()
	request.ID = stored.RequestID
	request.RequestedAt = stored.RequestedAt
	request.Client = client
	request.RequestedScope = stored.RequestedScope
	// Без granted scope проверка прав на защищённых эндпоинтах не сработает.
	request.GrantedScope = stored.GrantedScope
	request.RequestedAudience = stored.RequestedAudience
	request.GrantedAudience = stored.GrantedAudience
	request.Form = stored.Form
	request.Session = session
	return request, stored.ClientID, nil
}

func (s *ds) createTokenSession(ctx context.Context, tokenType, signature string, request fosite.Requester) error {
	data, err := encodeTokenSession(request)
	if err != nil {
		return fmt.Errorf("encoding %s token session: %w", tokenType, err)
	}

	// Срок жизни берётся по типу токена: у refresh он на порядок длиннее
	// access, и подстановка чужого срока обрывала refresh-поток через час.
	expiresAt := expiryOf(request, tokenType)
	_, err = s.db.ExecContext(ctx,
		"INSERT INTO oauth_tokens (signature, request_id, session_data, expires_at, token_type) VALUES ($1, $2, $3, $4, $5)",
		signature,
		request.GetID(),
		data,
		expiresAt,
		tokenType,
	)
	if err != nil {
		return fmt.Errorf("storing %s token session: %w", tokenType, err)
	}
	return nil
}

func tokenKind(tokenType string) fosite.TokenType {
	switch tokenType {
	case "refresh":
		return fosite.RefreshToken
	case "code", "pkce":
		return fosite.AuthorizeCode
	default:
		return fosite.AccessToken
	}
}

// defaultSessionLifespan — запас на случай, когда стратегия не проставила
// срок жизни в сессии. Без него такая запись считалась бы просроченной сразу.
const defaultSessionLifespan = 10 * time.Minute

func expiryOf(request fosite.Requester, tokenType string) time.Time {
	if expires := request.GetSession().GetExpiresAt(tokenKind(tokenType)); !expires.IsZero() {
		return expires
	}
	return time.Now().Add(defaultSessionLifespan)
}

func (s *ds) getTokenSession(ctx context.Context, tokenType, signature string, session fosite.Session) (fosite.Requester, error) {
	var data []byte
	var expiresAt time.Time

	err := s.db.QueryRowContext(ctx,
		"SELECT session_data, expires_at FROM oauth_tokens WHERE signature = $1 AND token_type = $2",
		signature,
		tokenType,
	).Scan(&data, &expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fosite.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("loading %s token session: %w", tokenType, err)
	}
	if expiresAt.Before(time.Now()) {
		return nil, fosite.ErrTokenExpired
	}

	// Первый проход нужен, чтобы узнать клиента: он хранится по
	// идентификатору, а не встроенным объектом.
	_, clientId, err := decodeTokenSession(data, nil, nil)
	if err != nil {
		return nil, fmt.Errorf("decoding %s token session: %w", tokenType, err)
	}
	client, err := s.GetClient(ctx, clientId)
	if err != nil {
		return nil, fmt.Errorf("loading client %s of %s token: %w", clientId, tokenType, err)
	}

	request, _, err := decodeTokenSession(data, session, client)
	if err != nil {
		return nil, fmt.Errorf("decoding %s token session: %w", tokenType, err)
	}
	return request, nil
}

// getCodeSession отличается от getTokenSession тем, что читает признак
// использования и не отбрасывает просроченный код молча: fosite сам решает,
// что делать с найденным, но невалидным кодом.
func (s *ds) getCodeSession(ctx context.Context, code string, session fosite.Session) (fosite.Requester, bool, error) {
	var data []byte
	var expiresAt time.Time
	var used bool

	err := s.db.QueryRowContext(ctx,
		"SELECT session_data, expires_at, used FROM oauth_tokens WHERE signature = $1 AND token_type = 'code'",
		code,
	).Scan(&data, &expiresAt, &used)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, fosite.ErrNotFound
	}
	if err != nil {
		return nil, false, fmt.Errorf("loading authorize code session: %w", err)
	}

	_, clientId, err := decodeTokenSession(data, nil, nil)
	if err != nil {
		return nil, false, fmt.Errorf("decoding authorize code session: %w", err)
	}
	client, err := s.GetClient(ctx, clientId)
	if err != nil {
		return nil, false, fmt.Errorf("loading client %s of authorize code: %w", clientId, err)
	}

	request, _, err := decodeTokenSession(data, session, client)
	if err != nil {
		return nil, false, fmt.Errorf("decoding authorize code session: %w", err)
	}
	if !used && expiresAt.Before(time.Now()) {
		return request, false, fosite.ErrTokenExpired
	}
	return request, used, nil
}

func (s *ds) deleteTokenSession(ctx context.Context, tokenType, signature string) error {
	_, err := s.db.ExecContext(ctx,
		"DELETE FROM oauth_tokens WHERE signature = $1 AND token_type = $2",
		signature,
		tokenType,
	)
	if err != nil {
		return fmt.Errorf("deleting %s token session: %w", tokenType, err)
	}
	return nil
}

func (s *ds) RevokeRefreshToken(ctx context.Context, requestId string) error {
	_, err := s.db.ExecContext(ctx,
		"DELETE FROM oauth_tokens WHERE request_id = $1 AND token_type = 'refresh'",
		requestId,
	)
	return err
}

func (s *ds) RevokeAccessToken(ctx context.Context, requestId string) error {
	_, err := s.db.ExecContext(ctx,
		"DELETE FROM oauth_tokens WHERE request_id = $1 AND token_type = 'access'",
		requestId,
	)
	return err
}

func (s *ds) IsUniqueConstraintError(err error) bool {
	return services.IsUniqueViolation(err)
}

func (s *ds) ClientAssertionJWTValid(ctx context.Context, jti string) error {
	return nil
}

func (s *ds) SetClientAssertionJWT(ctx context.Context, jti string, exp time.Time) error {
	return nil
}

func splitCSV(input string) []string {
	return strings.Split(input, ",")
}

func (s *ds) Stats() sql.DBStats {
	return s.db.Stats()
}

func (s *ds) Ping(ctx context.Context) error {
	return s.db.PingContext(ctx)
}

func (s *ds) Close() error {
	return s.db.Close()
}

func NewDatabaseUsers(ctx context.Context, cfg services.Config) (DatabaseUsers, error) {
	d, err := services.NewDatabase(ctx, cfg, migrations)
	if err != nil {
		return nil, fmt.Errorf("opening users database: %w", err)
	}

	store := &ds{db: d}
	if cfg.KeyMasterKey == "" {
		slog.Warn("KEY_MASTER_KEY is not set, signing keys are stored in plain text: " +
			"a database dump is enough to forge any token")
	} else if store.cipher, err = services.NewCipher(cfg.KeyMasterKey); err != nil {
		return nil, fmt.Errorf("creating key cipher: %w", err)
	}
	return store, nil
}

func (d ds) CreateConfirmation(
	ctx context.Context,
	confirmation Confirmation,
) (Confirmation, error) {
	created, err := scanConfirmation(d.db.QueryRowContext(ctx, `
		INSERT INTO confirmation (user_id, kind, target, code_hash, expires_at)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id, user_id, kind, target, code_hash, attempts, expires_at, confirmed_at, created_at`,
		confirmation.UserId, confirmation.Kind, confirmation.Target,
		confirmation.CodeHash, confirmation.ExpiresAt))
	if err != nil {
		return Confirmation{}, fmt.Errorf("creating %s confirmation for %s: %w",
			confirmation.Kind, confirmation.UserId, err)
	}
	return created, nil
}

func (d ds) ActiveConfirmation(
	ctx context.Context,
	user uuid.UUID,
	kind ConfirmationKind,
) (Confirmation, error) {
	confirmation, err := scanConfirmation(d.db.QueryRowContext(ctx, `
		SELECT id, user_id, kind, target, code_hash, attempts, expires_at, confirmed_at, created_at
		FROM confirmation
		WHERE user_id = $1 AND kind = $2 AND confirmed_at IS NULL
		  AND expires_at > current_timestamp AND attempts < $3
		ORDER BY created_at DESC
		LIMIT 1`, user, kind, MaxAttempts))
	if errors.Is(err, sql.ErrNoRows) {
		return Confirmation{}, ErrNoConfirmation
	}
	if err != nil {
		return Confirmation{}, fmt.Errorf("loading %s confirmation of %s: %w", kind, user, err)
	}
	return confirmation, nil
}

func (d ds) CountConfirmations(
	ctx context.Context,
	user uuid.UUID,
	kind ConfirmationKind,
	window time.Duration,
) (int, time.Time, error) {
	var (
		count int
		last  sql.NullTime
	)
	err := d.db.QueryRowContext(ctx, `
		SELECT count(*), max(created_at)
		FROM confirmation
		WHERE user_id = $1 AND kind = $2
		  AND created_at > current_timestamp - $3::interval`,
		user, kind, fmt.Sprintf("%d seconds", int(window.Seconds()))).Scan(&count, &last)
	if err != nil {
		return 0, time.Time{}, fmt.Errorf("counting %s confirmations of %s: %w", kind, user, err)
	}
	return count, last.Time, nil
}

func (d ds) FailConfirmation(ctx context.Context, id uuid.UUID) error {
	if _, err := d.db.ExecContext(ctx,
		`UPDATE confirmation SET attempts = attempts + 1 WHERE id = $1`, id); err != nil {
		return fmt.Errorf("counting failed attempt of confirmation %s: %w", id, err)
	}
	return nil
}

func (d ds) ConfirmContact(
	ctx context.Context,
	id, user uuid.UUID,
	kind ConfirmationKind,
	target string,
) error {
	return d.inTx(ctx, func(tx *sql.Tx) error {
		// Контакт сверяется в той же транзакции: между проверкой кода
		// и отметкой пользователь мог сменить номер, и подтверждать
		// тогда нечего.
		var current sql.NullString
		column := "phone"
		if kind == ConfirmEmail {
			column = "email"
		}
		if err := tx.QueryRowContext(ctx,
			`SELECT `+column+` FROM users WHERE user_id = $1 FOR UPDATE`, user).Scan(&current); err != nil {
			return fmt.Errorf("locking user %s: %w", user, err)
		}
		if !strings.EqualFold(current.String, target) {
			return ErrTargetChanged
		}

		if _, err := tx.ExecContext(ctx,
			`UPDATE users SET `+column+`_confirmed = TRUE, updated_at = current_timestamp
			 WHERE user_id = $1`, user); err != nil {
			return fmt.Errorf("confirming %s of user %s: %w", kind, user, err)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE confirmation SET confirmed_at = current_timestamp WHERE id = $1`, id); err != nil {
			return fmt.Errorf("marking confirmation %s used: %w", id, err)
		}
		return nil
	})
}

func scanConfirmation(scanner interface{ Scan(...any) error }) (Confirmation, error) {
	var (
		confirmation Confirmation
		confirmedAt  sql.NullTime
	)
	if err := scanner.Scan(&confirmation.Id, &confirmation.UserId, &confirmation.Kind,
		&confirmation.Target, &confirmation.CodeHash, &confirmation.Attempts,
		&confirmation.ExpiresAt, &confirmedAt, &confirmation.CreatedAt); err != nil {
		return Confirmation{}, err
	}
	if confirmedAt.Valid {
		confirmation.ConfirmedAt = &confirmedAt.Time
	}
	return confirmation, nil
}

func (d ds) inTx(ctx context.Context, do func(tx *sql.Tx) error) error {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("starting transaction: %w", err)
	}
	defer func() {
		// Откат после успешной фиксации возвращает ErrTxDone и ничего
		// не меняет, поэтому проверять его нечего.
		_ = tx.Rollback()
	}()

	if err = do(tx); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("committing transaction: %w", err)
	}
	return nil
}

// Identity — внешняя идентичность пользователя.
type Identity struct {
	Provider   string    `json:"provider"`
	ExternalId string    `json:"external_id"`
	Email      string    `json:"email,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
}

// SocialLogin — начатый вход через внешнего провайдера.
type SocialLogin struct {
	State          string
	Provider       string
	Verifier       string
	AuthorizeQuery string
	// LinkUserId заполнен, если вход начат ради привязки к уже
	// существующему пользователю.
	LinkUserId *uuid.UUID
	ExpiresAt  time.Time
}

func (d ds) StartSocialLogin(ctx context.Context, login SocialLogin) error {
	_, err := d.db.ExecContext(ctx, `
		INSERT INTO social_login (state, provider, verifier, authorize_query, link_user_id, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		login.State, login.Provider, login.Verifier, login.AuthorizeQuery,
		nullableUUID(login.LinkUserId), login.ExpiresAt)
	if err != nil {
		return fmt.Errorf("starting social login with %s: %w", login.Provider, err)
	}
	return nil
}

// TakeSocialLogin забирает состояние входа: оно одноразовое, поэтому
// удаляется тем же запросом. Иначе перехваченный ответ провайдера можно
// было бы предъявить повторно.
func (d ds) TakeSocialLogin(ctx context.Context, state string) (SocialLogin, error) {
	var (
		login SocialLogin
		link  sql.NullString
	)
	err := d.db.QueryRowContext(ctx, `
		DELETE FROM social_login
		WHERE state = $1 AND expires_at > current_timestamp
		RETURNING state, provider, verifier, authorize_query, link_user_id, expires_at`, state).
		Scan(&login.State, &login.Provider, &login.Verifier,
			&login.AuthorizeQuery, &link, &login.ExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return SocialLogin{}, ErrSocialState
	}
	if err != nil {
		return SocialLogin{}, fmt.Errorf("taking social login state: %w", err)
	}
	if link.Valid {
		parsed, err := uuid.Parse(link.String)
		if err != nil {
			return SocialLogin{}, fmt.Errorf("parsing link user id: %w", err)
		}
		login.LinkUserId = &parsed
	}
	return login, nil
}

// DeleteExpiredSocialLogins убирает брошенные состояния: пользователь
// мог начать вход и закрыть вкладку.
func (d ds) DeleteExpiredSocialLogins(ctx context.Context) (int64, error) {
	result, err := d.db.ExecContext(ctx,
		`DELETE FROM social_login WHERE expires_at <= current_timestamp`)
	if err != nil {
		return 0, fmt.Errorf("deleting expired social logins: %w", err)
	}
	deleted, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("counting deleted social logins: %w", err)
	}
	return deleted, nil
}

// IdentityUser находит пользователя по внешней идентичности.
func (d ds) IdentityUser(ctx context.Context, provider, externalId string) (uuid.UUID, error) {
	var user uuid.UUID
	err := d.db.QueryRowContext(ctx,
		`SELECT user_id FROM identity WHERE provider = $1 AND external_id = $2`,
		provider, externalId).Scan(&user)
	if errors.Is(err, sql.ErrNoRows) {
		return uuid.Nil, sql.ErrNoRows
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("loading identity %s/%s: %w", provider, externalId, err)
	}
	return user, nil
}

// LinkIdentity связывает внешний аккаунт с пользователем.
func (d ds) LinkIdentity(ctx context.Context, user uuid.UUID, profile SocialProfile) error {
	result, err := d.db.ExecContext(ctx, `
		INSERT INTO identity (provider, external_id, user_id, email)
		VALUES ($1, $2, $3, NULLIF($4, ''))
		ON CONFLICT (provider, external_id) DO NOTHING`,
		profile.Provider, profile.ExternalId, user, profile.Email)
	if err != nil {
		return fmt.Errorf("linking identity %s/%s: %w", profile.Provider, profile.ExternalId, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("counting linked identities: %w", err)
	}
	if affected == 0 {
		// Строка уже есть: либо привязка повторная, либо аккаунт связан
		// с другим пользователем — и это разные ответы.
		var owner uuid.UUID
		if owner, err = d.IdentityUser(ctx, profile.Provider, profile.ExternalId); err != nil {
			return err
		}
		if owner != user {
			return ErrIdentityTaken
		}
	}
	return nil
}

// Identities перечисляет способы внешнего входа пользователя.
func (d ds) Identities(ctx context.Context, user uuid.UUID) ([]Identity, error) {
	rows, err := d.db.QueryContext(ctx, `
		SELECT provider, external_id, COALESCE(email, ''), created_at
		FROM identity WHERE user_id = $1 ORDER BY created_at`, user)
	if err != nil {
		return nil, fmt.Errorf("loading identities of %s: %w", user, err)
	}
	defer func() {
		// Настоящая причина сбоя придёт из rows.Err().
		_ = rows.Close()
	}()

	identities := make([]Identity, 0)
	for rows.Next() {
		var identity Identity
		if err = rows.Scan(&identity.Provider, &identity.ExternalId,
			&identity.Email, &identity.CreatedAt); err != nil {
			return nil, fmt.Errorf("scanning identity: %w", err)
		}
		identities = append(identities, identity)
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("reading identities: %w", err)
	}
	return identities, nil
}

// UnlinkIdentity отвязывает внешний аккаунт.
//
// Последний способ входа отвязать нельзя: пользователь, вошедший через
// провайдера и не заводивший пароль, потерял бы доступ к учётной записи.
func (d ds) UnlinkIdentity(ctx context.Context, user uuid.UUID, provider string) error {
	return d.inTx(ctx, func(tx *sql.Tx) error {
		var passwordSet bool
		if err := tx.QueryRowContext(ctx,
			`SELECT password_set FROM users WHERE user_id = $1 FOR UPDATE`, user).
			Scan(&passwordSet); err != nil {
			return fmt.Errorf("locking user %s: %w", user, err)
		}

		var remaining int
		if err := tx.QueryRowContext(ctx,
			`SELECT count(*) FROM identity WHERE user_id = $1 AND provider <> $2`,
			user, provider).Scan(&remaining); err != nil {
			return fmt.Errorf("counting identities of %s: %w", user, err)
		}
		if !passwordSet && remaining == 0 {
			return ErrLastIdentity
		}

		result, err := tx.ExecContext(ctx,
			`DELETE FROM identity WHERE user_id = $1 AND provider = $2`, user, provider)
		if err != nil {
			return fmt.Errorf("unlinking identity %s of %s: %w", provider, user, err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("counting unlinked identities: %w", err)
		}
		if affected == 0 {
			return sql.ErrNoRows
		}
		return nil
	})
}

// CreateSocialUser заводит пользователя по внешнему профилю.
//
// Почта провайдера в профиль не переносится: она может совпасть с почтой
// существующего пользователя, и тогда чужой аккаунт оказался бы захвачен
// тем, кто просто завёл ящик с таким адресом у провайдера. Почта остаётся
// справочной в самой идентичности, а в профиль пользователь заведёт её сам
// и подтвердит.
func (d ds) CreateSocialUser(ctx context.Context, profile SocialProfile) (*User, error) {
	var user *User
	err := d.inTx(ctx, func(tx *sql.Tx) error {
		// Пароля нет: вход возможен только через провайдера, пока
		// пользователь не задаст пароль сам.
		placeholder, err := randomToken()
		if err != nil {
			return err
		}

		created, err := scanUser(tx.QueryRowContext(ctx, `
			INSERT INTO users (username, password_hash, display_name, password_set)
			VALUES ($1, $2, NULLIF($3, ''), FALSE)
			RETURNING `+userColumns,
			profile.Provider+":"+profile.ExternalId, placeholder, profile.Name))
		if err != nil {
			return fmt.Errorf("creating user from %s identity: %w", profile.Provider, err)
		}

		if _, err = tx.ExecContext(ctx, `
			INSERT INTO identity (provider, external_id, user_id, email)
			VALUES ($1, $2, $3, NULLIF($4, ''))`,
			profile.Provider, profile.ExternalId, created.Id, profile.Email); err != nil {
			return fmt.Errorf("linking identity to the new user: %w", err)
		}

		user = created
		return nil
	})
	return user, err
}

func nullableUUID(value *uuid.UUID) any {
	if value == nil {
		return nil
	}
	return *value
}
