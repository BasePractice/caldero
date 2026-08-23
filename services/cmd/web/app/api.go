//go:build js && wasm

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// appConfig приходит от сервиса раздачи: адрес API и идентификатор клиента
// зависят от стенда, а бандл один и тот же.
type appConfig struct {
	API      string `json:"api"`
	ClientId string `json:"client_id"`
}

// client — доступ к API.
//
// Токен хранится только в поле структуры, то есть в памяти вкладки:
// в localStorage его читает любой скрипт на странице, а перезагрузка
// с повторным входом — цена, которую за это стоит платить.
type client struct {
	config appConfig
	token  string
	http   *http.Client
}

func newClient(config appConfig) *client {
	// Под js/wasm net/http работает поверх fetch, поэтому таймаут задаётся
	// здесь: иначе запрос висит до закрытия вкладки.
	return &client{config: config, http: &http.Client{Timeout: 30 * time.Second}}
}

func (c *client) authorized() bool { return c.token != "" }

// exchange меняет код авторизации на токен доступа.
func (c *client) exchange(ctx context.Context, code string) error {
	verifier := sessionValue(verifierKey)
	if verifier == "" {
		return errText("вход не был начат в этой вкладке")
	}

	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", redirectURI())
	form.Set("client_id", c.config.ClientId)
	form.Set("code_verifier", verifier)

	request, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.config.API+"/token", strings.NewReader(form.Encode()))
	if err != nil {
		return errText("не удалось подготовить запрос токена")
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	var token struct {
		AccessToken string `json:"access_token"`
	}
	if err = c.do(request, &token); err != nil {
		return err
	}
	if token.AccessToken == "" {
		return errText("сервер не выдал токен")
	}

	c.token = token.AccessToken
	// Проверочный код одноразовый: он больше не нужен и не должен
	// оставаться в хранилище вкладки.
	clearSessionValue(verifierKey)
	clearSessionValue(stateKey)
	return nil
}

// get читает ресурс API.
func (c *client) get(ctx context.Context, path string, into any) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.config.API+path, nil)
	if err != nil {
		return errText("не удалось подготовить запрос")
	}
	return c.do(request, into)
}

// post отправляет ресурс в API.
func (c *client) post(ctx context.Context, path string, body, into any) error {
	encoded, err := json.Marshal(body)
	if err != nil {
		return errText("не удалось подготовить данные")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.config.API+path, bytes.NewReader(encoded))
	if err != nil {
		return errText("не удалось подготовить запрос")
	}
	request.Header.Set("Content-Type", "application/json")
	return c.do(request, into)
}

func (c *client) do(request *http.Request, into any) error {
	if c.token != "" {
		request.Header.Set("Authorization", "Bearer "+c.token)
	}

	response, err := c.http.Do(request)
	if err != nil {
		return errText("сервис недоступен")
	}
	defer func() {
		// Тело читается ниже; здесь остаётся только закрыть.
		_ = response.Body.Close()
	}()

	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return errText("не удалось прочитать ответ")
	}

	switch {
	case response.StatusCode == http.StatusUnauthorized:
		// Токен живёт час; после этого нужен новый вход.
		c.token = ""
		return errText("нужно войти заново")
	case response.StatusCode >= 400:
		message := strings.TrimSpace(string(body))
		if message == "" {
			message = response.Status
		}
		return errText(fmt.Sprintf("сервис ответил: %s", message))
	}

	if into == nil || len(body) == 0 {
		return nil
	}
	if err = json.Unmarshal(body, into); err != nil {
		return errText("не удалось разобрать ответ сервиса")
	}
	return nil
}
