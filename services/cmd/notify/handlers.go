package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"wish/services"
	"wish/services/shared/notify"

	"github.com/google/uuid"
)

const (
	// maxWait ограничивает длинный опрос. Значение подобрано под таймаут
	// записи HTTP-сервера: ожидание дольше него оборвалось бы на середине.
	maxWait = 20 * time.Second
	// defaultLimit и maxLimit ограничивают страницу ленты.
	defaultLimit = 50
	maxLimit     = 200
)

// api собирает обработчики. Структура, а не набор замыканий: зависимостей
// уже четыре, и передавать их каждому обработчику отдельно значит
// повторять один и тот же список.
type api struct {
	db  Database
	hub *Hub
	// messengers — подключённые боты по каналам. Привязка у них общая,
	// различаются только адреса и имена полей.
	messengers map[notify.Channel]*Messenger
	// email нужен обработчику отписки: ссылку подписывает он же.
	email     *Email
	codeTTL   time.Duration
	wsOrigins []string
}

func registerHttpHandlers(a *api) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /notify/events", a.publish)
	mux.HandleFunc("GET /notify/messages", a.messages)
	mux.HandleFunc("GET /notify/preferences", a.preferences)
	mux.HandleFunc("PUT /notify/preferences", a.setPreferences)
	mux.HandleFunc("POST /notify/messengers/{provider}/link", a.linkMessenger)
	mux.HandleFunc("GET /notify/messengers/{provider}", a.messengerState)
	mux.HandleFunc("GET /notify/unsubscribe", a.unsubscribe)
	mux.HandleFunc("GET /notify/ws", func(w http.ResponseWriter, r *http.Request) {
		serveWebSocket(a.hub, a.wsOrigins, w, r)
	})
	return services.Measure("notify", mux)
}

// publish кладёт событие в очередь и сразу отвечает.
//
// Ответ 202, а не 200: доставка асинхронна намеренно. Отправка в Telegram
// не должна ни задерживать бизнес-операцию, ни откатывать её при сбое —
// подарок остаётся подаренным, даже если бот недоступен.
func (a *api) publish(w http.ResponseWriter, r *http.Request) {
	operator, err := services.HttpAuthorized(r)
	if err != nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	event, err := services.DecodeJSON[notify.PublishEvent](w, r)
	if err != nil {
		services.WriteDecodeError(w, err)
		return
	}
	if err = event.Validate(); err != nil {
		slog.DebugContext(r.Context(), "Event validation failed",
			slog.String("event", event.String()), slog.String("reason", err.Error()))
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	// Оповещение чужому пользователю рассылает сервис, действующий
	// от имени оператора: иначе любой авторизованный мог бы слать
	// сообщения кому угодно от имени системы.
	if !operator.CanActOnBehalfOf(event.UserId) {
		slog.WarnContext(r.Context(), "Attempt to notify another user",
			slog.String("operator", operator.Id.String()))
		http.Error(w, "Notifying another user requires the operator role", http.StatusForbidden)
		return
	}

	channels, err := a.db.EnabledChannels(r.Context(), event.UserId, event.Type)
	if err != nil {
		slog.ErrorContext(r.Context(), "Can't load enabled channels", slog.String("err", err.Error()))
		http.Error(w, "Can't publish event", http.StatusInternalServerError)
		return
	}

	published, duplicate, err := a.db.Publish(r.Context(), event, channels)
	if err != nil {
		slog.ErrorContext(r.Context(), "Can't publish event",
			slog.String("event", event.String()), slog.String("err", err.Error()))
		http.Error(w, "Can't publish event", http.StatusInternalServerError)
		return
	}

	w.Header().Set("X-Event-Id", published.Id.String())
	writeJSON(r.Context(), w, http.StatusAccepted, map[string]any{
		"id": published.Id,
		// duplicate сообщает публикующему сервису, что его повтор распознан,
		// а не создал второе сообщение.
		"duplicate": duplicate,
		"channels":  channels,
	})
}

// messages отдаёт ленту по курсору и, если ничего нового нет, ждёт
// появления — тот самый «пулинг как в вк».
func (a *api) messages(w http.ResponseWriter, r *http.Request) {
	authorized, err := services.HttpAuthorized(r)
	if err != nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	after, err := intParam(r, "after", 0)
	if err != nil {
		http.Error(w, "Invalid after", http.StatusBadRequest)
		return
	}
	requested, err := intParam(r, "limit", defaultLimit)
	if err != nil || requested <= 0 {
		http.Error(w, "Invalid limit", http.StatusBadRequest)
		return
	}
	limit := int(min(requested, maxLimit))
	wait, err := waitParam(r)
	if err != nil {
		http.Error(w, "Invalid wait", http.StatusBadRequest)
		return
	}

	messages, err := a.db.Messages(r.Context(), authorized.Id, after, limit)
	if err != nil {
		slog.ErrorContext(r.Context(), "Can't load messages", slog.String("err", err.Error()))
		http.Error(w, "Can't load messages", http.StatusInternalServerError)
		return
	}

	if len(messages) == 0 && wait > 0 {
		// Подписка оформляется до повторного чтения: сообщение, попавшее
		// в ленту между запросами, иначе не разбудило бы ожидание,
		// и клиент прождал бы полный цикл впустую.
		updates, unsubscribe := a.hub.Subscribe(authorized.Id)
		defer unsubscribe()

		if messages, err = a.db.Messages(r.Context(), authorized.Id, after, limit); err != nil {
			slog.ErrorContext(r.Context(), "Can't load messages", slog.String("err", err.Error()))
			http.Error(w, "Can't load messages", http.StatusInternalServerError)
			return
		}
		if len(messages) == 0 {
			timer := time.NewTimer(wait)
			select {
			case <-r.Context().Done():
				timer.Stop()
				return
			case <-timer.C:
			case <-updates:
				timer.Stop()
				// Читается снова из базы, а не берётся из подписки:
				// пока запрос ждал, могло появиться несколько сообщений,
				// и порядок ленты задаёт база.
				if messages, err = a.db.Messages(r.Context(), authorized.Id, after, limit); err != nil {
					slog.ErrorContext(r.Context(), "Can't load messages", slog.String("err", err.Error()))
					http.Error(w, "Can't load messages", http.StatusInternalServerError)
					return
				}
			}
		}
	}

	cursor := after
	if len(messages) > 0 {
		cursor = messages[len(messages)-1].Seq
	}
	writeJSON(r.Context(), w, http.StatusOK, map[string]any{
		"messages": messages,
		"cursor":   cursor,
	})
}

func (a *api) preferences(w http.ResponseWriter, r *http.Request) {
	authorized, err := services.HttpAuthorized(r)
	if err != nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	preferences, err := a.db.Preferences(r.Context(), authorized.Id)
	if err != nil {
		slog.ErrorContext(r.Context(), "Can't load preferences", slog.String("err", err.Error()))
		http.Error(w, "Can't load preferences", http.StatusInternalServerError)
		return
	}
	writeJSON(r.Context(), w, http.StatusOK, preferences)
}

func (a *api) setPreferences(w http.ResponseWriter, r *http.Request) {
	authorized, err := services.HttpAuthorized(r)
	if err != nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	preferences, err := services.DecodeJSON[[]notify.Preference](w, r)
	if err != nil {
		services.WriteDecodeError(w, err)
		return
	}
	if len(preferences) > len(notify.EventTypes())*len(notify.Channels()) {
		http.Error(w, "Too many preferences", http.StatusBadRequest)
		return
	}
	for _, preference := range preferences {
		if err = preference.Validate(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}

	for _, preference := range preferences {
		if err = a.db.SetPreference(r.Context(), authorized.Id, preference); err != nil {
			slog.ErrorContext(r.Context(), "Can't save preference", slog.String("err", err.Error()))
			http.Error(w, "Can't save preferences", http.StatusInternalServerError)
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *api) linkMessenger(w http.ResponseWriter, r *http.Request) {
	authorized, err := services.HttpAuthorized(r)
	if err != nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	messenger, ok := a.messenger(w, r)
	if !ok {
		return
	}

	code, err := NewBindingCode(rand.Read)
	if err != nil {
		slog.ErrorContext(r.Context(), "Can't generate binding code", slog.String("err", err.Error()))
		http.Error(w, "Can't start binding", http.StatusInternalServerError)
		return
	}
	expires := time.Now().Add(a.codeTTL)
	if err = a.db.StartMessengerBinding(r.Context(), messenger.Channel(), authorized.Id,
		messenger.BindingCodeHash(code), expires); err != nil {
		slog.ErrorContext(r.Context(), "Can't start messenger binding", slog.String("err", err.Error()))
		http.Error(w, "Can't start binding", http.StatusInternalServerError)
		return
	}

	writeJSON(r.Context(), w, http.StatusOK, map[string]any{
		"provider":   messenger.Channel(),
		"code":       code,
		"link":       BindingLink(messenger.config.BotName, code),
		"expires_at": expires,
	})
}

func (a *api) messengerState(w http.ResponseWriter, r *http.Request) {
	authorized, err := services.HttpAuthorized(r)
	if err != nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	messenger, ok := a.messenger(w, r)
	if !ok {
		return
	}

	binding, err := a.db.MessengerBinding(r.Context(), messenger.Channel(), authorized.Id)
	if errors.Is(err, ErrNotFound) {
		writeJSON(r.Context(), w, http.StatusOK, map[string]any{"bound": false})
		return
	}
	if err != nil {
		slog.ErrorContext(r.Context(), "Can't load messenger binding", slog.String("err", err.Error()))
		http.Error(w, "Can't load binding", http.StatusInternalServerError)
		return
	}
	// Идентификатор чата наружу не отдаётся: пользователю он не нужен,
	// а в ответе API это лишние данные о его аккаунте в мессенджере.
	writeJSON(r.Context(), w, http.StatusOK, map[string]any{
		"bound":    true,
		"blocked":  binding.Blocked,
		"bound_at": binding.BoundAt,
	})
}

// unsubscribe выключает письма по ссылке из письма.
//
// Ссылка подписана: без подписи достаточно подставить чужой идентификатор,
// чтобы отписать постороннего. Метод GET намеренно поддержан вместе
// с POST — почтовые клиенты открывают ссылку обычным переходом.
func (a *api) unsubscribe(w http.ResponseWriter, r *http.Request) {
	if a.email == nil {
		http.Error(w, "Email channel is not configured", http.StatusNotFound)
		return
	}

	user, err := uuid.Parse(r.URL.Query().Get("user"))
	if err != nil {
		http.Error(w, "Invalid link", http.StatusBadRequest)
		return
	}
	if !a.email.VerifyUnsubscribe(user, r.URL.Query().Get("sign")) {
		slog.WarnContext(r.Context(), "Unsubscribe link with a wrong signature",
			slog.String("user", user.String()))
		http.Error(w, "Invalid link", http.StatusBadRequest)
		return
	}

	// Отписка выключает канал целиком для всех событий: человек, нажавший
	// «отписаться», просит не писать ему больше, а не настроить фильтры.
	for _, eventType := range notify.EventTypes() {
		if err = a.db.SetPreference(r.Context(), user, notify.Preference{
			Type: eventType, Channel: notify.ChannelEmail, Enabled: false,
		}); err != nil {
			slog.ErrorContext(r.Context(), "Can't unsubscribe", slog.String("err", err.Error()))
			http.Error(w, "Can't unsubscribe", http.StatusInternalServerError)
			return
		}
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, `<!doctype html><html lang="ru"><head><meta charset="utf-8">`+
		`<title>Отписка</title></head><body>`+
		`<p>Письма отключены. Включить их снова можно в настройках оповещений.</p>`+
		`</body></html>`)
}

// messenger достаёт бота из пути. Неизвестный и ненастроенный канал —
// одно и то же для клиента.
func (a *api) messenger(w http.ResponseWriter, r *http.Request) (*Messenger, bool) {
	provider := notify.Channel(strings.ToUpper(r.PathValue("provider")))
	messenger, ok := a.messengers[provider]
	if !ok {
		http.Error(w, "Messenger is not configured", http.StatusNotFound)
		return nil, false
	}
	return messenger, true
}

func intParam(r *http.Request, name string, fallback int64) (int64, error) {
	value := r.URL.Query().Get(name)
	if value == "" {
		return fallback, nil
	}
	return strconv.ParseInt(value, 10, 64)
}

func waitParam(r *http.Request) (time.Duration, error) {
	value := r.URL.Query().Get("wait")
	if value == "" {
		return 0, nil
	}
	seconds, err := strconv.Atoi(value)
	if err != nil || seconds < 0 {
		return 0, errors.New("wait must be a non-negative number of seconds")
	}
	wait := time.Duration(seconds) * time.Second
	if wait > maxWait {
		wait = maxWait
	}
	return wait, nil
}

func writeJSON(ctx context.Context, w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		// Ответ уже начат: сообщить об ошибке клиенту нечем.
		slog.ErrorContext(ctx, "Can't encode response", slog.String("err", err.Error()))
	}
}
