//go:build js && wasm

package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"net/url"
	"strings"
)

// Ключи в sessionStorage. Туда попадает только то, что должно пережить
// редирект к провайдеру авторизации.
const (
	verifierKey = "wish.pkce.verifier"
	stateKey    = "wish.pkce.state"
)

// login отправляет пользователя на страницу входа.
//
// Authorization Code Flow с PKCE: интерфейс — публичный клиент, секрета
// у него нет и быть не может, поэтому перехваченный код без проверочного
// кода обменять на токен нельзя.
func login(config appConfig) error {
	verifier, err := randomToken()
	if err != nil {
		return err
	}
	state, err := randomToken()
	if err != nil {
		return err
	}
	setSessionValue(verifierKey, verifier)
	setSessionValue(stateKey, state)

	sum := sha256.Sum256([]byte(verifier))
	query := url.Values{}
	query.Set("response_type", "code")
	query.Set("client_id", config.ClientId)
	query.Set("redirect_uri", redirectURI())
	// offline_access просится ради токена обновления: без него провайдер
	// его не выдаёт, и работа обрывалась бы через час.
	query.Set("scope", "openid read write offline_access")
	query.Set("state", state)
	query.Set("code_challenge", base64.RawURLEncoding.EncodeToString(sum[:]))
	query.Set("code_challenge_method", "S256")

	// Страница входа открывается с адреса самого интерфейса, а не через
	// шлюз: вход заканчивается перенаправлением с кодом, а шлюз его
	// не пропускает (EXT-10). Заодно адрес возврата совпадает с origin
	// приложения, и браузер весь вход остаётся на одном источнике.
	location().Call("assign", "/auth?"+query.Encode())
	return nil
}

// redirectURI — адрес возврата: та же страница без параметров.
func redirectURI() string {
	current := location()
	return current.Get("origin").String() + current.Get("pathname").String()
}

// callbackCode достаёт код авторизации из адресной строки и сверяет state.
//
// Без сверки state ответ подделывается: злоумышленник подсовывает свой код
// и связывает свою сессию с чужим окном.
func callbackCode() (string, error) {
	query, err := url.ParseQuery(strings.TrimPrefix(location().Get("search").String(), "?"))
	if err != nil {
		return "", errText("не удалось разобрать адрес возврата")
	}
	code := query.Get("code")
	if code == "" {
		return "", nil
	}
	if query.Get("state") != sessionValue(stateKey) {
		return "", errText("ответ не соответствует начатому входу")
	}
	return code, nil
}

// clearQuery убирает код из адресной строки: перезагрузка страницы
// не должна пытаться обменять уже использованный код.
func clearQuery() {
	js := location()
	history := jsGlobalHistory()
	if history.IsUndefined() {
		return
	}
	history.Call("replaceState", nil, "", js.Get("pathname").String())
}

func randomToken() (string, error) {
	buffer := make([]byte, 32)
	if _, err := rand.Read(buffer); err != nil {
		return "", errText("не удалось подготовить вход")
	}
	return base64.RawURLEncoding.EncodeToString(buffer), nil
}
