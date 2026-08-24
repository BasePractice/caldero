package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// SocialProvider — внешний провайдер входа.
//
// Провайдер задаётся конфигурацией, а не кодом: VK, Яндекс, Google и прочие
// различаются адресами и названиями полей, но не потоком. Писать под каждого
// свой адаптер значило бы копировать один и тот же обмен кода на токен.
type SocialProvider struct {
	Name         string
	ClientId     string
	ClientSecret string
	AuthURL      string
	TokenURL     string
	UserInfoURL  string
	Scopes       string
	// Поля профиля у провайдеров называются по-разному, поэтому пути
	// к ним тоже конфигурация. Путь — через точку: "response.email".
	IdField    string
	EmailField string
	NameField  string
}

// SocialProfile — то, что удалось узнать о пользователе у провайдера.
type SocialProfile struct {
	Provider   string
	ExternalId string
	Email      string
	Name       string
}

// Ошибки внешнего входа.
var (
	// ErrUnknownProvider — провайдер не настроен.
	ErrUnknownProvider = errors.New("social provider is not configured")
	// ErrSocialState — состояние входа не найдено или просрочено.
	// Для пользователя это «начните заново», а не подробности.
	ErrSocialState = errors.New("social login state is unknown or expired")
	// ErrSocialProvider — провайдер ответил ошибкой.
	ErrSocialProvider = errors.New("social provider refused the request")
	// ErrIdentityTaken — внешний аккаунт уже связан с другим пользователем.
	ErrIdentityTaken = errors.New("identity is already linked to another user")
	// ErrLastIdentity — отвязать нечего: это единственный способ входа.
	ErrLastIdentity = errors.New("this is the only way to sign in")
)

// socialStateTTL ограничивает время на внешний вход: столько живёт
// начатое состояние.
const socialStateTTL = 10 * time.Minute

// socialTimeout ограничивает обращение к провайдеру.
const socialTimeout = 10 * time.Second

// LoadSocialProviders читает провайдеров из окружения.
//
// Разбор живёт в сервисе, а не в общей конфигурации: набор переменных
// зависит от числа провайдеров и нужен только здесь. К этому моменту
// .env уже загружен общим стартом сервиса.
func LoadSocialProviders(names []string) (map[string]SocialProvider, error) {
	providers := make(map[string]SocialProvider, len(names))
	for _, name := range names {
		name = strings.ToLower(strings.TrimSpace(name))
		if name == "" {
			continue
		}

		prefix := "SOCIAL_" + strings.ToUpper(name) + "_"
		provider := SocialProvider{
			Name:         name,
			ClientId:     os.Getenv(prefix + "CLIENT_ID"),
			ClientSecret: os.Getenv(prefix + "CLIENT_SECRET"),
			AuthURL:      os.Getenv(prefix + "AUTH_URL"),
			TokenURL:     os.Getenv(prefix + "TOKEN_URL"),
			UserInfoURL:  os.Getenv(prefix + "USERINFO_URL"),
			Scopes:       os.Getenv(prefix + "SCOPES"),
			IdField:      envOr(prefix+"ID_FIELD", "id"),
			EmailField:   envOr(prefix+"EMAIL_FIELD", "email"),
			NameField:    envOr(prefix+"NAME_FIELD", "name"),
		}
		if err := provider.Validate(); err != nil {
			return nil, err
		}
		providers[name] = provider
	}
	return providers, nil
}

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

// Validate возвращает причину отказа: провайдер без адреса токена или
// секрета не заработает, и узнать об этом лучше при старте.
func (p SocialProvider) Validate() error {
	missing := make([]string, 0, 5)
	for field, value := range map[string]string{
		"CLIENT_ID":     p.ClientId,
		"CLIENT_SECRET": p.ClientSecret,
		"AUTH_URL":      p.AuthURL,
		"TOKEN_URL":     p.TokenURL,
		"USERINFO_URL":  p.UserInfoURL,
	} {
		if value == "" {
			missing = append(missing, field)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("social provider %s is missing: %s", p.Name, strings.Join(missing, ", "))
	}
	return nil
}

// AuthorizeURL собирает адрес, на который отправляется пользователь.
func (p SocialProvider) AuthorizeURL(redirectURI, state, challenge string) string {
	query := url.Values{}
	query.Set("response_type", "code")
	query.Set("client_id", p.ClientId)
	query.Set("redirect_uri", redirectURI)
	query.Set("state", state)
	if p.Scopes != "" {
		query.Set("scope", p.Scopes)
	}
	// PKCE отправляется всем: провайдер, который его не поддерживает,
	// лишний параметр игнорирует, а перехваченный код без проверочного
	// кода обменивается на токен кем угодно.
	query.Set("code_challenge", challenge)
	query.Set("code_challenge_method", "S256")

	separator := "?"
	if strings.Contains(p.AuthURL, "?") {
		separator = "&"
	}
	return p.AuthURL + separator + query.Encode()
}

// NewSocialSecrets выдаёт state и пару PKCE.
func NewSocialSecrets() (state, verifier, challenge string, err error) {
	if state, err = randomToken(); err != nil {
		return "", "", "", err
	}
	if verifier, err = randomToken(); err != nil {
		return "", "", "", err
	}
	sum := sha256.Sum256([]byte(verifier))
	return state, verifier, base64.RawURLEncoding.EncodeToString(sum[:]), nil
}

func randomToken() (string, error) {
	buffer := make([]byte, 32)
	if _, err := rand.Read(buffer); err != nil {
		return "", fmt.Errorf("generating social login token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buffer), nil
}

// SocialClient обменивает код на токен и читает профиль.
type SocialClient struct {
	client *http.Client
}

func NewSocialClient() *SocialClient {
	return &SocialClient{client: &http.Client{Timeout: socialTimeout}}
}

// Exchange меняет код авторизации на токен доступа.
func (c *SocialClient) Exchange(
	ctx context.Context,
	provider SocialProvider,
	code, verifier, redirectURI string,
) (string, error) {
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", redirectURI)
	form.Set("client_id", provider.ClientId)
	form.Set("client_secret", provider.ClientSecret)
	form.Set("code_verifier", verifier)

	request, err := http.NewRequestWithContext(ctx, http.MethodPost,
		provider.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("creating token request: %w", err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Accept", "application/json")

	body, err := c.call(request, provider.Name+" token")
	if err != nil {
		return "", err
	}

	var token struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
	}
	if err = json.Unmarshal(body, &token); err != nil {
		return "", fmt.Errorf("%w: decoding token response: %w", ErrSocialProvider, err)
	}
	if token.AccessToken == "" {
		return "", fmt.Errorf("%w: no access token (%s)", ErrSocialProvider, token.Error)
	}
	return token.AccessToken, nil
}

// Profile читает профиль пользователя у провайдера.
func (c *SocialClient) Profile(
	ctx context.Context,
	provider SocialProvider,
	accessToken string,
) (SocialProfile, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, provider.UserInfoURL, nil)
	if err != nil {
		return SocialProfile{}, fmt.Errorf("creating userinfo request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+accessToken)
	request.Header.Set("Accept", "application/json")

	body, err := c.call(request, provider.Name+" userinfo")
	if err != nil {
		return SocialProfile{}, err
	}

	var raw map[string]any
	if err = json.Unmarshal(body, &raw); err != nil {
		return SocialProfile{}, fmt.Errorf("%w: decoding userinfo: %w", ErrSocialProvider, err)
	}

	external := field(raw, provider.IdField)
	if external == "" {
		// Без идентификатора связывать нечего: почта у провайдера
		// меняется и повторяется, а этот идентификатор — нет.
		return SocialProfile{}, fmt.Errorf("%w: userinfo has no %s", ErrSocialProvider, provider.IdField)
	}
	return SocialProfile{
		Provider:   provider.Name,
		ExternalId: external,
		Email:      strings.ToLower(field(raw, provider.EmailField)),
		Name:       field(raw, provider.NameField),
	}, nil
}

func (c *SocialClient) call(request *http.Request, what string) ([]byte, error) {
	response, err := c.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %w", ErrSocialProvider, what, err)
	}
	defer func() {
		// Тело читается ниже; здесь остаётся только закрыть.
		_ = response.Body.Close()
	}()

	// Ответ читается с ограничением: его размер задаёт чужой сервис.
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("%w: reading %s: %w", ErrSocialProvider, what, err)
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: %s answered %s", ErrSocialProvider, what, response.Status)
	}
	return body, nil
}

// field достаёт значение по пути через точку.
//
// Поля профиля у провайдеров лежат на разной глубине, и путь — это
// конфигурация, а не код. Числа приводятся к строке: идентификатор
// у одних строка, у других число, и различать их незачем.
func field(raw map[string]any, path string) string {
	if path == "" {
		return ""
	}

	var current any = raw
	for _, part := range strings.Split(path, ".") {
		object, ok := current.(map[string]any)
		if !ok {
			return ""
		}
		current, ok = object[part]
		if !ok {
			return ""
		}
	}

	switch value := current.(type) {
	case string:
		return value
	case float64:
		return strconv.FormatFloat(value, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(value)
	default:
		return ""
	}
}
