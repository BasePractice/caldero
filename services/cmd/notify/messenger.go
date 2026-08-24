package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"wish/services/shared/notify"
)

const (
	// botTimeout ограничивает обычный запрос к Bot API.
	botTimeout = 10 * time.Second
	// botPollTimeout — сколько Bot API держит запрос обновлений, пока их
	// нет. Длинный опрос дешевле частых коротких запросов. Значение
	// укладывается в пределы обеих площадок: Telegram ограничений
	// не задаёт, МАКС принимает от 0 до 90 секунд.
	botPollTimeout = 25 * time.Second
)

// bot — общая часть настроек бота. Различия площадок живут в диалекте,
// а канал, адрес, токен и имя бота нужны одинаково всем.
type bot struct {
	channel notify.Channel
	api     string
	token   string
	name    string
}

// botUpdate — обновление в том виде, в каком оно нужно привязке. Площадки
// сообщают о событиях по-разному, но привязке нужны чат, текст команды
// и признак того, что бота запустили или остановили.
type botUpdate struct {
	ChatId int64
	// Text — текст сообщения или команда, собранная из другого события:
	// в МАКС код привязки приходит не сообщением, а полем payload
	// события запуска бота.
	Text string
	// Started и Stopped — пользователь запустил или остановил бота.
	// Telegram о таких событиях не сообщает вовсе, и там оба признака
	// всегда ложны.
	Started bool
	Stopped bool
}

// dialect — протокол конкретной площадки.
//
// Общее у ботов только действия: отправить сообщение, забрать обновления,
// дать ссылку привязки. Всё остальное различается, и подстановкой имён
// полей это не сводится: у Telegram токен в пути, получатель в теле
// запроса и ответ в конверте `{ok, result}`, у МАКС токен в заголовке,
// получатель в строке запроса, а ответ — сам результат.
type dialect interface {
	// Send отправляет текст в чат.
	Send(ctx context.Context, chatId int64, text string) error
	// Updates забирает порцию обновлений начиная с курсора и возвращает
	// курсор для следующего запроса. Что такое курсор, решает площадка:
	// у Telegram это номер следующего обновления, у МАКС — метка,
	// присланная в ответе.
	Updates(ctx context.Context, cursor int64) ([]botUpdate, int64, error)
	// BindingLink — ссылка, по которой бот получает код без ручного ввода.
	BindingLink(code string) string
}

// Messenger — доставка ботом и привязка аккаунта. Привязка написана один
// раз на все площадки: различает их только диалект.
type Messenger struct {
	db      Database
	bot     bot
	dialect dialect
}

func (t *Messenger) Channel() notify.Channel { return t.bot.channel }

// BindingLink собирает ссылку, по которой бот получает код без ручного ввода.
func (t *Messenger) BindingLink(code string) string { return t.dialect.BindingLink(code) }

// BindingCodeHash считает хеш кода привязки.
//
// В базе лежит хеш, а не код: список действующих кодов — это список
// готовых способов привязать чужой аккаунт к своему боту. Ключом служит
// токен бота: смена токена обесценивает старые коды, и это правильно —
// они всё равно живут минуты.
func (t *Messenger) BindingCodeHash(code string) []byte {
	mac := hmac.New(sha256.New, []byte(t.bot.token))
	// hash.Hash по контракту не возвращает ошибку записи.
	_, _ = mac.Write([]byte(strings.ToUpper(code)))
	return mac.Sum(nil)
}

func (t *Messenger) Send(ctx context.Context, task Task, title, body string) error {
	binding, err := t.db.MessengerBinding(ctx, t.bot.channel, task.UserId)
	if errors.Is(err, ErrNotFound) {
		return ErrChannelUnbound
	}
	if err != nil {
		return fmt.Errorf("loading %s binding: %w", t.bot.channel, err)
	}
	if binding.Blocked {
		return ErrChannelBlocked
	}

	// Разметка не используется: в тексте оказываются названия товаров
	// и имена, введённые людьми, и любая разметка на них ломается.
	err = t.dialect.Send(ctx, binding.ChatId, title+"\n\n"+body)
	if errors.Is(err, ErrChannelBlocked) {
		// Бот заблокирован пользователем. Отмечаем это, иначе каждое
		// следующее событие будет заново упираться в тот же отказ.
		if blockErr := t.db.BlockMessenger(ctx, t.bot.channel, task.UserId); blockErr != nil {
			slog.ErrorContext(ctx, "Can't mark messenger blocked",
				slog.String("user", task.UserId.String()), slog.String("err", blockErr.Error()))
		}
		return ErrChannelBlocked
	}
	return err
}

// Run обрабатывает обновления бота до отмены контекста: по команде
// /start с кодом привязывает чат к пользователю.
func (t *Messenger) Run(ctx context.Context) error {
	var cursor int64
	for {
		if ctx.Err() != nil {
			//nolint:nilerr // отмена контекста — штатная остановка бота, а не сбой
			return nil
		}

		updates, next, err := t.dialect.Updates(ctx, cursor)
		if err != nil {
			if ctx.Err() != nil {
				//nolint:nilerr // сбой на фоне отмены — следствие остановки, а не её причина
				return nil
			}
			slog.WarnContext(ctx, "Can't read messenger updates",
				slog.String("channel", string(t.bot.channel)), slog.String("err", err.Error()))
			// Пауза после сбоя: без неё цикл превращается в непрерывный
			// поток запросов к недоступному API.
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(botTimeout):
			}
			continue
		}
		// Курсор сдвигается и тогда, когда все обновления пропущены:
		// неподтверждённые обновления площадка повторяет бесконечно.
		cursor = next

		for _, update := range updates {
			switch {
			case update.Stopped:
				// Площадка сама сообщила, что бота остановили. Ждать
				// отказа отправки незачем: он всё равно был бы позже.
				t.setBlocked(ctx, update.ChatId, true)
			case update.Started:
				// Бота запустили снова: без снятия отметки оповещения
				// в этот чат больше не пошли бы никогда.
				t.setBlocked(ctx, update.ChatId, false)
				t.handleCommand(ctx, update.ChatId, update.Text)
			default:
				t.handleCommand(ctx, update.ChatId, update.Text)
			}
		}
	}
}

// setBlocked отмечает блокировку по чату: о запуске и остановке бота
// площадка сообщает чатом, а не нашим пользователем.
func (t *Messenger) setBlocked(ctx context.Context, chatId int64, blocked bool) {
	if err := t.db.SetMessengerBlocked(ctx, t.bot.channel, chatId, blocked); err != nil {
		slog.ErrorContext(ctx, "Can't change messenger blocked mark",
			slog.String("channel", string(t.bot.channel)), slog.String("err", err.Error()))
	}
}

func (t *Messenger) handleCommand(ctx context.Context, chatId int64, text string) {
	fields := strings.Fields(text)
	if len(fields) == 0 || !strings.HasPrefix(fields[0], "/start") {
		t.reply(ctx, chatId, "Чтобы получать оповещения, откройте привязку в приложении и пришлите команду /start с кодом.")
		return
	}
	if len(fields) < 2 {
		t.reply(ctx, chatId, "Нужен код привязки: /start КОД. Код выдаётся в настройках оповещений.")
		return
	}

	user, err := t.db.CompleteMessengerBinding(ctx, t.bot.channel, t.BindingCodeHash(fields[1]), chatId)
	if errors.Is(err, ErrNotFound) {
		// Причина не уточняется: по разнице ответов «код не найден»
		// и «код просрочен» подбирать код удобнее.
		t.reply(ctx, chatId, "Код не подошёл. Проверьте его в приложении или получите новый.")
		return
	}
	if err != nil {
		slog.ErrorContext(ctx, "Can't complete messenger binding",
			slog.String("channel", string(t.bot.channel)), slog.String("err", err.Error()))
		t.reply(ctx, chatId, "Не получилось привязать аккаунт. Попробуйте позже.")
		return
	}

	slog.InfoContext(ctx, "Messenger bound",
		slog.String("channel", string(t.bot.channel)), slog.String("user", user.String()))
	t.reply(ctx, chatId, "Аккаунт привязан. Оповещения будут приходить сюда.")
}

func (t *Messenger) reply(ctx context.Context, chatId int64, text string) {
	if err := t.dialect.Send(ctx, chatId, text); err != nil {
		slog.WarnContext(ctx, "Can't reply in messenger", slog.String("err", err.Error()))
	}
}

// newBotClient — клиент для запросов к Bot API. Таймаут запроса больше
// времени длинного опроса: иначе клиент обрывал бы каждое ожидание
// обновлений.
func newBotClient() *http.Client {
	return &http.Client{Timeout: botPollTimeout + botTimeout}
}

// truncateMessage укорачивает текст под предел площадки. Предел считается
// в знаках, а не в байтах: обрезка по байтам разрубила бы кириллицу
// посередине символа.
func truncateMessage(text string, limit int) string {
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit])
}

// NewBindingCode выдаёт код привязки. Алфавит без похожих знаков: код
// переписывают руками, и различить 0 и O в мессенджере невозможно.
func NewBindingCode(random func([]byte) (int, error)) (string, error) {
	const alphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	const length = 8

	buffer := make([]byte, length)
	if _, err := random(buffer); err != nil {
		return "", fmt.Errorf("generating binding code: %w", err)
	}
	code := make([]byte, length)
	for i, b := range buffer {
		code[i] = alphabet[int(b)%len(alphabet)]
	}
	return string(code), nil
}

// LoadMessengers читает настройки ботов из окружения.
//
// Разбор живёт в сервисе, а не в общей конфигурации: набор переменных
// зависит от числа мессенджеров и нужен только здесь. Токен служит
// выключателем: без него бот не поднимается, всё остальное известно
// из документации площадки и задано значениями по умолчанию.
func LoadMessengers(db Database, telegramToken, telegramAPI, telegramBot string) map[notify.Channel]*Messenger {
	messengers := make(map[notify.Channel]*Messenger, 2)

	if telegramToken != "" {
		messengers[notify.ChannelTelegram] = NewTelegram(db, telegramToken, telegramAPI, telegramBot)
	}
	if token := os.Getenv("NOTIFY_MAX_TOKEN"); token != "" {
		messengers[notify.ChannelMax] = NewMax(db, token,
			os.Getenv("NOTIFY_MAX_API"), os.Getenv("NOTIFY_MAX_BOT"))
	}
	return messengers
}
