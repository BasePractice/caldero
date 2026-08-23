package main

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"wish/services"

	"github.com/google/uuid"
	"github.com/lib/pq"
	_ "github.com/lib/pq"
	"github.com/ory/fosite"
	"github.com/ory/fosite/handler/oauth2"
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

	CreateUser(ctx context.Context, username, passwordHash string) (*User, error)
	GetUser(ctx context.Context, username string) (*User, error)

	GetLastKey(ctx context.Context) (string, error)
	GetKey(ctx context.Context, id string) ([]byte, error)
	GetKeys(ctx context.Context, cb func(string, []byte)) error
	CreateKey(ctx context.Context, key []byte) (string, error)
	ClearKeys(ctx context.Context) error

	IsUniqueConstraintError(err error) bool
	CreateClient(ctx context.Context, clientId, clientSecret, redirectUri, scopes, responseType, grantTypes string)
}

type ds struct {
	db *sql.DB
}

func (s *ds) CreateAuthorizeCodeSession(ctx context.Context, code string, request fosite.Requester) (err error) {
	//TODO implement me
	panic("implement me")
}

func (s *ds) GetAuthorizeCodeSession(ctx context.Context, code string, session fosite.Session) (request fosite.Requester, err error) {
	//TODO implement me
	panic("implement me")
}

func (s *ds) InvalidateAuthorizeCodeSession(ctx context.Context, code string) (err error) {
	//TODO implement me
	panic("implement me")
}

func (s *ds) RotateRefreshToken(ctx context.Context, requestID string, refreshTokenSignature string) (err error) {
	//TODO implement me
	panic("implement me")
}

func (s *ds) CreateClient(ctx context.Context, clientId, clientSecret, redirectUri, scopes, responseType, grantTypes string) {
	_, err := s.db.ExecContext(ctx, "INSERT INTO oauth_clients (client_id, client_secret, redirect_uris, grant_types, response_types, scopes) VALUES ($1, $2, $3, $4, $5, $6)", clientId, clientSecret, redirectUri, grantTypes, responseType, scopes)
	if err != nil {
		slog.Error("Failed to create client", slog.String("error", err.Error()))
	}
}

func (s *ds) GetKeys(ctx context.Context, cb func(string, []byte)) error {
	rows, err := s.db.QueryContext(ctx, "SELECT key_id, private_key FROM keys ORDER BY created_at DESC LIMIT 2")
	if err != nil {
		return err
	}
	for rows.Next() {
		var id string
		var privateKey []byte
		err = rows.Scan(&id, &privateKey)
		if err != nil {
			return err
		}
		cb(id, privateKey)
	}
	return nil
}

func (s *ds) GetKey(ctx context.Context, id string) ([]byte, error) {
	var privateKey []byte
	err := s.db.QueryRowContext(ctx, "SELECT private_key FROM keys WHERE key_id = $1", id).Scan(&privateKey)
	if err != nil {
		return nil, err
	}
	return privateKey, nil
}

func (s *ds) CreateKey(ctx context.Context, key []byte) (string, error) {
	keyId := fmt.Sprintf("key-%d", time.Now().UnixNano())
	_, err := s.db.ExecContext(ctx, "INSERT INTO keys (key_id, private_key) VALUES ($1, $2)", keyId, key)
	if err != nil {
		return "", err
	}
	return keyId, nil
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

	err := s.db.QueryRow(
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
	return s.createTokenSession("access", signature, request)
}

func (s *ds) GetAccessTokenSession(ctx context.Context, signature string, session fosite.Session) (fosite.Requester, error) {
	return s.getTokenSession("access", signature, session)
}

func (s *ds) DeleteAccessTokenSession(ctx context.Context, signature string) error {
	return s.deleteTokenSession("access", signature)
}

func (s *ds) CreateRefreshTokenSession(ctx context.Context, signature string, accessSignature string, request fosite.Requester) (err error) {
	return s.createTokenSession("refresh", signature, request)
}

func (s *ds) GetRefreshTokenSession(ctx context.Context, signature string, session fosite.Session) (fosite.Requester, error) {
	return s.getTokenSession("refresh", signature, session)
}

func (s *ds) DeleteRefreshTokenSession(ctx context.Context, signature string) error {
	return s.deleteTokenSession("refresh", signature)
}

func (s *ds) createTokenSession(tokenType, signature string, request fosite.Requester) error {
	data, err := json.Marshal(request)
	if err != nil {
		return fmt.Errorf("marshalling %s token session: %w", tokenType, err)
	}

	// Срок жизни берётся по типу токена: у refresh он на порядок длиннее
	// access, и подстановка чужого срока обрывала refresh-поток через час.
	expiresAt := request.GetSession().GetExpiresAt(tokenKind(tokenType))
	_, err = s.db.Exec(
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
	if tokenType == "refresh" {
		return fosite.RefreshToken
	}
	return fosite.AccessToken
}

func (s *ds) getTokenSession(tokenType, signature string, session fosite.Session) (fosite.Requester, error) {
	var data []byte
	var expiresAt time.Time

	err := s.db.QueryRow(
		"SELECT session_data, expires_at FROM oauth_tokens WHERE signature = $1 AND token_type = $2",
		signature,
		tokenType,
	).Scan(&data, &expiresAt)

	if err != nil {
		return nil, err
	}

	var req fosite.Request
	if err := json.Unmarshal(data, &req); err != nil {
		return nil, err
	}

	if expiresAt.Before(time.Now()) {
		return nil, fosite.ErrTokenExpired
	}

	return &req, nil
}

func (s *ds) deleteTokenSession(tokenType, signature string) error {
	_, err := s.db.Exec(
		"DELETE FROM oauth_tokens WHERE signature = $1 AND token_type = $2",
		signature,
		tokenType,
	)
	return err
}

func (s *ds) RevokeRefreshToken(ctx context.Context, requestId string) error {
	_, err := s.db.Exec(
		"DELETE FROM oauth_tokens WHERE request_id = $1 AND token_type = 'refresh'",
		requestId,
	)
	return err
}

func (s *ds) RevokeAccessToken(ctx context.Context, requestId string) error {
	_, err := s.db.Exec(
		"DELETE FROM oauth_tokens WHERE request_id = $1 AND token_type = 'access'",
		requestId,
	)
	return err
}

func (s *ds) IsUniqueConstraintError(err error) bool {
	var pge *pq.Error
	if errors.As(err, &pge) {
		return pge.Code == "23505"
	}
	return false
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

func NewDatabaseUsers(cfg services.Config) DatabaseUsers {
	d, err := services.NewDatabase(cfg, migrations)
	if err != nil {
		return nil
	}
	return &ds{d}
}
