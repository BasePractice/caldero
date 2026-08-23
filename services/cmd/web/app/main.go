//go:build js && wasm

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"wish/services/shared/marketplace"
	"wish/services/shared/notify"
	"wish/services/shared/wishlist"
)

// Интерфейс на Go под WebAssembly. Модели общие с сервисами: типы ниже —
// те же самые, что сервер отдаёт в JSON, поэтому расхождение между
// клиентом и сервером ловится компилятором, а не в браузере.

var api *client

func main() {
	ctx := context.Background()

	config, err := loadConfig(ctx)
	if err != nil {
		fail(err.Error())
		return
	}
	api = newClient(config)

	code, err := callbackCode()
	if err != nil {
		fail(err.Error())
		return
	}
	if code != "" {
		clearQuery()
		if err = api.exchange(ctx, code); err != nil {
			fail(err.Error())
			renderGuest()
			select {}
		}
	}

	if api.authorized() {
		renderUser(ctx)
	} else {
		renderGuest()
	}

	// Приложение живёт до закрытия вкладки: обработчики событий должны
	// оставаться живыми, а main — не завершаться.
	select {}
}

// loadConfig читает адрес API у сервиса раздачи.
func loadConfig(ctx context.Context) (appConfig, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "config.json", nil)
	if err != nil {
		return appConfig{}, errText("не удалось запросить настройки")
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return appConfig{}, errText("не удалось получить настройки")
	}
	defer func() {
		_ = response.Body.Close()
	}()

	var config appConfig
	if err = json.NewDecoder(response.Body).Decode(&config); err != nil {
		return appConfig{}, errText("настройки не разбираются")
	}
	if config.API == "" {
		return appConfig{}, errText("в настройках нет адреса API")
	}
	return config, nil
}

// renderGuest показывает экран для не вошедшего пользователя.
func renderGuest() {
	status("Вы не вошли.")
	setHTML("app", `<button id="login">Войти</button>`)
	onClick("login", func() {
		if err := login(api.config); err != nil {
			fail(err.Error())
		}
	})
}

// renderUser собирает основной экран: профиль, список желаний и лента.
func renderUser(ctx context.Context) {
	status("Загрузка данных…")

	var profile struct {
		Username       string `json:"username"`
		DisplayName    string `json:"display_name"`
		Phone          string `json:"phone"`
		PhoneConfirmed bool   `json:"phone_confirmed"`
		Email          string `json:"email"`
		EmailConfirmed bool   `json:"email_confirmed"`
	}
	if err := api.get(ctx, "/profile", &profile); err != nil {
		fail(err.Error())
		renderGuest()
		return
	}

	name := profile.DisplayName
	if name == "" {
		name = profile.Username
	}

	markup := &strings.Builder{}
	markup.WriteString(`<h2>Профиль</h2><div class="card">`)
	fmt.Fprintf(markup, `<div><strong>%s</strong></div>`, escape(name))
	fmt.Fprintf(markup, `<div class="muted">Телефон: %s — %s</div>`,
		escape(orDash(profile.Phone)), confirmed(profile.PhoneConfirmed))
	fmt.Fprintf(markup, `<div class="muted">Почта: %s — %s</div>`,
		escape(orDash(profile.Email)), confirmed(profile.EmailConfirmed))
	markup.WriteString(`</div>`)

	markup.WriteString(`<h2>Список желаний</h2>`)
	markup.WriteString(`<div class="row">
		<input id="product" placeholder="Идентификатор товара" size="24">
		<select id="priority">
			<option value="1">Очень хочу</option>
			<option value="3" selected>Хочу</option>
			<option value="5">Пусть будет</option>
		</select>
		<button id="add">Добавить</button>
	</div><div id="items"></div>`)

	markup.WriteString(`<h2>Оповещения</h2><div id="messages"></div>`)
	setHTML("app", markup.String())
	status("")

	onClick("add", func() { addItem(ctx) })
	loadItems(ctx)
	loadMessages(ctx)
}

// loadItems показывает список желаний.
func loadItems(ctx context.Context) {
	var items []wishlist.Item
	if err := api.get(ctx, "/wishlist/items", &items); err != nil {
		setHTML("items", `<span class="error">`+escape(err.Error())+`</span>`)
		return
	}
	if len(items) == 0 {
		setHTML("items", `<p class="muted">Список пуст.</p>`)
		return
	}

	markup := &strings.Builder{}
	for _, item := range items {
		markup.WriteString(`<div class="card">`)
		fmt.Fprintf(markup, `<div><strong>%s</strong> — %s</div>`,
			escape(item.Title), escape(stateText(item.State)))
		if item.Kind == wishlist.KindProduct {
			fmt.Fprintf(markup, `<div class="muted">Цена на момент добавления: %s ₽</div>`,
				escape(item.Price.String()))
		} else {
			fmt.Fprintf(markup, `<div class="muted">Сумма: %s ₽</div>`, escape(item.Amount.String()))
		}
		markup.WriteString(`</div>`)
	}
	setHTML("items", markup.String())
}

// addItem добавляет товар в список желаний.
func addItem(ctx context.Context) {
	product := strings.TrimSpace(value("product"))
	if product == "" {
		fail("Укажите идентификатор товара")
		return
	}
	priority, err := strconv.Atoi(value("priority"))
	if err != nil {
		priority = 3
	}

	// Тип запроса общий с сервером: поля не разъедутся молча.
	create := wishlist.CreateItem{
		Kind:      wishlist.KindProduct,
		Priority:  priority,
		Provider:  marketplace.ProviderStub,
		ProductId: product,
	}
	if err = api.post(ctx, "/wishlist/items", create, nil); err != nil {
		fail(err.Error())
		return
	}

	status("Добавлено.")
	loadItems(ctx)
}

// loadMessages показывает ленту оповещений.
//
// Через шлюз доступен только длинный опрос: KrakenD Community Edition
// не проксирует WebSocket (EXT-05). Здесь опрос без ожидания —
// одна страница, один запрос.
func loadMessages(ctx context.Context) {
	var feed struct {
		Messages []notify.Message `json:"messages"`
		Cursor   int64            `json:"cursor"`
	}
	if err := api.get(ctx, "/notify/messages?after=0&limit=10", &feed); err != nil {
		setHTML("messages", `<span class="muted">`+escape(err.Error())+`</span>`)
		return
	}
	if len(feed.Messages) == 0 {
		setHTML("messages", `<p class="muted">Пока ничего не приходило.</p>`)
		return
	}

	markup := &strings.Builder{}
	for _, message := range feed.Messages {
		markup.WriteString(`<div class="card">`)
		fmt.Fprintf(markup, `<div><strong>%s</strong></div><div>%s</div>`,
			escape(message.Title), escape(message.Body))
		fmt.Fprintf(markup, `<div class="muted">%s</div>`,
			escape(message.CreatedAt.Format(time.RFC3339)))
		markup.WriteString(`</div>`)
	}
	setHTML("messages", markup.String())
}

func orDash(value string) string {
	if value == "" {
		return "—"
	}
	return value
}

func confirmed(ok bool) string {
	if ok {
		return "подтверждён"
	}
	return "не подтверждён"
}

// stateText переводит состояние элемента для показа. Состояния приходят
// из общей модели, поэтому новый вариант не потеряется молча.
func stateText(state wishlist.State) string {
	switch state {
	case wishlist.StateVisible:
		return "виден дарителям"
	case wishlist.StateHidden:
		return "скрыт"
	case wishlist.StateChosen:
		return "кто-то собирается подарить"
	case wishlist.StateConfirmed:
		return "подарок подтверждён"
	case wishlist.StateAccepted:
		return "подарен"
	case wishlist.StateRejected:
		return "отклонён"
	default:
		return string(state)
	}
}
