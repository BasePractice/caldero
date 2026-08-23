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

	CreateUser(ctx context.Context, username, passwordHash string) (*User, error)
	GetUser(ctx context.Context, username string) (*User, error)
	Authenticate(ctx context.Context, username, secret string) (string, error)
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

func (s *ds) GetUser(ctx context.Context, username string) (*User, error) {
	var u = User{Username: username}
	err := s.db.QueryRowContext(ctx, "SELECT user_id, password_hash FROM users WHERE username = $1", username).Scan(&u.Id, &u.PasswordHash)
	if err != nil {
		return nil, err
	}
	return &u, nil
}

// Authenticate требуется password-грантом. Неизвестный пользователь и неверный
// пароль дают один и тот же ответ: различие позволило бы перебирать логины.
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

func (s *ds) CreateUser(ctx context.Context, username, passwordHash string) (*User, error) {
	var id uuid.UUID
	err := s.db.QueryRowContext(ctx,
		"INSERT INTO users (username, password_hash) VALUES ($1, $2) RETURNING user_id",
		username, passwordHash,
	).Scan(&id)
	if err != nil {
		return nil, err
	}
	return &User{Id: id, Username: username, PasswordHash: passwordHash}, nil
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
