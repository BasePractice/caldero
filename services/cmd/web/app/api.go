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
// Токены хранятся только в полях структуры, то есть в памяти вкладки:
// в localStorage их читает любой скрипт на странице, а перезагрузка
// с повторным входом — цена, которую за это стоит платить.
//
// Токен доступа живёт час, поэтому рядом лежит токен обновления: без него
// работа обрывалась бы через час прямо посреди дела. Сессия при этом
// кончается вместе со вкладкой — дальше вкладки ни один из них не уходит.
type client struct {
	config  appConfig
	token   string
	refresh string
	http    *http.Client
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

	if err = c.keep(request); err != nil {
		return err
	}
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

// do выполняет запрос. Если токен доступа истёк, обновляет его и повторяет
// запрос один раз: иначе час работы заканчивался бы предложением войти
// заново на середине действия.
func (c *client) do(request *http.Request, into any) error {
	expired, err := c.send(c.authorize(request), into)
	if !expired {
		return err
	}

	if err = c.renew(request.Context()); err != nil {
		c.forget()
		return err
	}
	// Тело запроса уже прочитано: для повтора его нужно взять заново.
	if request.GetBody != nil {
		body, err := request.GetBody()
		if err != nil {
			return errText("не удалось повторить запрос")
		}
		request.Body = body
	}

	expired, err = c.send(c.authorize(request), into)
	if expired {
		// Свежий токен тоже не подошёл: дальше только вход.
		c.forget()
		return errText("нужно войти заново")
	}
	return err
}

// keep выполняет запрос за токенами и запоминает выданное.
func (c *client) keep(request *http.Request) error {
	var token struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	// Токен доступа сюда не идёт: провайдер ждёт в этом запросе
	// удостоверение клиента, а не пользователя.
	if _, err := c.send(request, &token); err != nil {
		return err
	}
	if token.AccessToken == "" {
		return errText("сервер не выдал токен")
	}
	c.token = token.AccessToken
	// Токен обновления выдаётся не всегда: без области offline_access
	// провайдер его не даёт, и тогда сессия просто кончится через час.
	c.refresh = token.RefreshToken
	return nil
}

// renew меняет токен обновления на новую пару.
func (c *client) renew(ctx context.Context) error {
	if c.refresh == "" {
		return errText("нужно войти заново")
	}

	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", c.refresh)
	form.Set("client_id", c.config.ClientId)

	request, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.config.API+"/token", strings.NewReader(form.Encode()))
	if err != nil {
		return errText("не удалось подготовить запрос токена")
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	// Прежний токен обновления одноразовый: провайдер выдаёт новый вместе
	// с токеном доступа, а старый отзывает.
	return c.keep(request)
}

// forget стирает токены: с этого момента приложение считается невошедшим.
func (c *client) forget() {
	c.token = ""
	c.refresh = ""
}

// authorize добавляет к запросу текущий токен доступа.
func (c *client) authorize(request *http.Request) *http.Request {
	if c.token != "" {
		request.Header.Set("Authorization", "Bearer "+c.token)
	}
	return request
}

// send выполняет один запрос. Истечение токена возвращается отдельным
// признаком, а не ошибкой: по нему do решает, повторять ли запрос.
func (c *client) send(request *http.Request, into any) (bool, error) {
	response, err := c.http.Do(request)
	if err != nil {
		return false, errText("сервис недоступен")
	}
	defer func() {
		// Тело читается ниже; здесь остаётся только закрыть.
		_ = response.Body.Close()
	}()

	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return false, errText("не удалось прочитать ответ")
	}

	switch {
	case response.StatusCode == http.StatusUnauthorized:
		return true, errText("нужно войти заново")
	case response.StatusCode >= 400:
		message := strings.TrimSpace(string(body))
		if message == "" {
			message = response.Status
		}
		return false, errText(fmt.Sprintf("сервис ответил: %s", message))
	}

	if into == nil || len(body) == 0 {
		return false, nil
	}
	if err = json.Unmarshal(body, into); err != nil {
		return false, errText("не удалось разобрать ответ сервиса")
	}
	return false, nil
}
