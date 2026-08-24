package main

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"wish/services/shared/notify"

	"github.com/google/uuid"
)

// fakeDatabase подменяет репозиторий в тестах доставки. Интерфейс встроен,
// а не реализован целиком: методы, которых тест не касается, вызываться
// не должны, и обращение к ним падает сразу, а не проходит незаметно.
type fakeDatabase struct {
	Database

	mu       sync.Mutex
	tasks    []Task
	sent     int
	messages []notify.Message

	delivered []uuid.UUID
	retried   []time.Duration
	failed    []string
	deferred  []time.Duration

	published         []notify.PublishEvent
	publishedChannels []notify.Channel
	publishErr        error
	feed              []notify.Message
	saved             []notify.Preference

	startedHash   []byte
	binding       MessengerBinding
	bindingErr    error
	blocked       bool
	blockedChat   int64
	completedHash []byte
	completedChat int64
	completeUser  uuid.UUID
	completeErr   error
}

func (f *fakeDatabase) Publish(
	_ context.Context,
	event notify.PublishEvent,
	channels []notify.Channel,
) (notify.Event, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.publishErr != nil {
		return notify.Event{}, false, f.publishErr
	}
	f.published = append(f.published, event)
	f.publishedChannels = channels
	return notify.Event{Id: uuid.New(), UserId: event.UserId, Type: event.Type}, false, nil
}

func (f *fakeDatabase) EnabledChannels(
	_ context.Context,
	_ uuid.UUID,
	_ notify.EventType,
) ([]notify.Channel, error) {
	return []notify.Channel{notify.ChannelInApp}, nil
}

func (f *fakeDatabase) Messages(_ context.Context, _ uuid.UUID, after int64, limit int) ([]notify.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	page := make([]notify.Message, 0, limit)
	for _, message := range f.feed {
		if message.Seq > after && len(page) < limit {
			page = append(page, message)
		}
	}
	return page, nil
}

// appendFeed добавляет сообщение в ленту так, как это сделал бы канал
// приложения: сначала запись, потом сигнал подписчикам.
func (f *fakeDatabase) appendFeed(message notify.Message) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.feed = append(f.feed, message)
}

func (f *fakeDatabase) Preferences(_ context.Context, _ uuid.UUID) ([]notify.Preference, error) {
	return []notify.Preference{{
		Type: notify.EventWishlistItemReserved, Channel: notify.ChannelInApp, Enabled: true,
	}}, nil
}

func (f *fakeDatabase) SetPreference(_ context.Context, _ uuid.UUID, preference notify.Preference) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.saved = append(f.saved, preference)
	return nil
}

func (f *fakeDatabase) MessengerBinding(
	_ context.Context,
	_ notify.Channel,
	_ uuid.UUID,
) (MessengerBinding, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.binding, f.bindingErr
}

func (f *fakeDatabase) BlockMessenger(_ context.Context, _ notify.Channel, _ uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.blocked = true
	return nil
}

func (f *fakeDatabase) SetMessengerBlocked(
	_ context.Context,
	_ notify.Channel,
	chatId int64,
	blocked bool,
) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.blocked = blocked
	f.blockedChat = chatId
	return nil
}

func (f *fakeDatabase) CompleteMessengerBinding(
	_ context.Context,
	_ notify.Channel,
	codeHash []byte,
	chatId int64,
) (uuid.UUID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.completedHash = codeHash
	f.completedChat = chatId
	return f.completeUser, f.completeErr
}

func (f *fakeDatabase) Claim(_ context.Context, _ int, _ time.Duration) ([]Task, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	tasks := f.tasks
	f.tasks = nil
	return tasks, nil
}

func (f *fakeDatabase) Delivered(_ context.Context, id uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.delivered = append(f.delivered, id)
	return nil
}

func (f *fakeDatabase) Retry(_ context.Context, _ uuid.UUID, after time.Duration, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.retried = append(f.retried, after)
	return nil
}

func (f *fakeDatabase) Failed(_ context.Context, _ uuid.UUID, reason string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failed = append(f.failed, reason)
	return nil
}

func (f *fakeDatabase) Defer(_ context.Context, _ uuid.UUID, after time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deferred = append(f.deferred, after)
	return nil
}

func (f *fakeDatabase) SentSince(_ context.Context, _ uuid.UUID, _ notify.Channel, _ time.Duration) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sent, nil
}

func (f *fakeDatabase) AppendMessage(_ context.Context, task Task, title, body string) (notify.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	message := notify.Message{
		Id: uuid.New(), Seq: int64(len(f.messages) + 1),
		Type: task.Type, Title: title, Body: body, CreatedAt: time.Now(),
	}
	f.messages = append(f.messages, message)
	return message, nil
}

// stubSender подменяет канал доставки.
type stubSender struct {
	channel notify.Channel
	err     error
	calls   int
}

func (s *stubSender) Channel() notify.Channel { return s.channel }

func (s *stubSender) Send(context.Context, Task, string, string) error {
	s.calls++
	return s.err
}

func testTask(attempts int) Task {
	return Task{
		Id: uuid.New(), EventId: uuid.New(), UserId: uuid.New(),
		Channel: notify.ChannelTelegram, Attempts: attempts,
		Type: notify.EventWishlistItemReserved, Payload: map[string]string{"item": "Кофеварка"},
	}
}

func newTestDispatcher(t *testing.T, db Database, senders ...Sender) *Dispatcher {
	t.Helper()
	templates, err := LoadTemplates()
	if err != nil {
		t.Fatalf("загрузка шаблонов: %v", err)
	}
	return NewDispatcher(db, templates, senders...)
}

func TestDispatcherDelivers(t *testing.T) {
	task := testTask(0)
	db := &fakeDatabase{tasks: []Task{task}}
	sender := &stubSender{channel: notify.ChannelTelegram}
	dispatcher := newTestDispatcher(t, db, sender)

	handled, err := dispatcher.Once(context.Background())
	if err != nil {
		t.Fatalf("проход доставки: %v", err)
	}
	if handled != 1 {
		t.Fatalf("обработано заданий %d, ожидалось 1", handled)
	}
	if len(db.delivered) != 1 || db.delivered[0] != task.Id {
		t.Errorf("задание не отмечено доставленным: %+v", db.delivered)
	}
}

func TestDispatcherRetriesTemporaryFailure(t *testing.T) {
	db := &fakeDatabase{tasks: []Task{testTask(0), testTask(2)}}
	sender := &stubSender{channel: notify.ChannelTelegram, err: ErrChannelUnavailable}
	dispatcher := newTestDispatcher(t, db, sender)

	if _, err := dispatcher.Once(context.Background()); err != nil {
		t.Fatalf("проход доставки: %v", err)
	}
	if len(db.retried) != 2 {
		t.Fatalf("отложено заданий %d, ожидалось 2 (%+v)", len(db.retried), db.failed)
	}
	// Задержка растёт с числом попыток: канал, не ответивший сейчас,
	// чаще всего не ответит и через секунду.
	if db.retried[1] <= db.retried[0] {
		t.Errorf("задержка не выросла: %s и %s", db.retried[0], db.retried[1])
	}
}

func TestDispatcherAbandonsPermanentFailure(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{"бот заблокирован", ErrChannelBlocked},
		{"канал не привязан", ErrChannelUnbound},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			db := &fakeDatabase{tasks: []Task{testTask(0)}}
			sender := &stubSender{channel: notify.ChannelTelegram, err: test.err}
			dispatcher := newTestDispatcher(t, db, sender)

			if _, err := dispatcher.Once(context.Background()); err != nil {
				t.Fatalf("проход доставки: %v", err)
			}
			if len(db.failed) != 1 {
				t.Fatalf("снято с доставки %d заданий, ожидалось 1", len(db.failed))
			}
			if len(db.retried) != 0 {
				t.Errorf("повторы на постоянной ошибке: %+v", db.retried)
			}
		})
	}
}

func TestDispatcherStopsAfterMaxAttempts(t *testing.T) {
	db := &fakeDatabase{tasks: []Task{testTask(defaultMaxAttempts - 1)}}
	sender := &stubSender{channel: notify.ChannelTelegram, err: ErrChannelUnavailable}
	dispatcher := newTestDispatcher(t, db, sender)

	if _, err := dispatcher.Once(context.Background()); err != nil {
		t.Fatalf("проход доставки: %v", err)
	}
	if len(db.failed) != 1 {
		t.Errorf("задание не снято с доставки после предела попыток: %+v", db.retried)
	}
}

// TestDispatcherRateLimit проверяет, что превышение частоты откладывает
// сообщение, а не тратит на него попытки: иначе поток смен состояния
// котла сжёг бы все попытки и сообщения пропали бы.
func TestDispatcherRateLimit(t *testing.T) {
	db := &fakeDatabase{tasks: []Task{testTask(0)}, sent: defaultRateLimit}
	sender := &stubSender{channel: notify.ChannelTelegram}
	dispatcher := newTestDispatcher(t, db, sender)

	if _, err := dispatcher.Once(context.Background()); err != nil {
		t.Fatalf("проход доставки: %v", err)
	}
	if len(db.deferred) != 1 {
		t.Fatalf("отложено %d заданий, ожидалось 1", len(db.deferred))
	}
	if sender.calls != 0 {
		t.Errorf("канал вызван %d раз при превышении частоты", sender.calls)
	}
	if len(db.retried) != 0 || len(db.failed) != 0 {
		t.Errorf("превышение частоты потратило попытку: %+v %+v", db.retried, db.failed)
	}
}

func TestDispatcherFailsUnknownChannel(t *testing.T) {
	db := &fakeDatabase{tasks: []Task{testTask(0)}}
	// Канал Telegram не подключён: токена бота нет.
	dispatcher := newTestDispatcher(t, db, &stubSender{channel: notify.ChannelInApp})

	if _, err := dispatcher.Once(context.Background()); err != nil {
		t.Fatalf("проход доставки: %v", err)
	}
	if len(db.failed) != 1 {
		t.Errorf("задание неподключённого канала осталось в очереди: %+v", db.failed)
	}
}

func TestDispatcherFailsBrokenTemplate(t *testing.T) {
	task := testTask(0)
	task.Payload = nil // Обязательной подстановки нет.
	db := &fakeDatabase{tasks: []Task{task}}
	sender := &stubSender{channel: notify.ChannelTelegram}
	dispatcher := newTestDispatcher(t, db, sender)

	if _, err := dispatcher.Once(context.Background()); err != nil {
		t.Fatalf("проход доставки: %v", err)
	}
	if len(db.failed) != 1 {
		t.Fatalf("сообщение без подстановки не снято с доставки")
	}
	if sender.calls != 0 {
		t.Errorf("канал вызван %d раз при непригодном шаблоне", sender.calls)
	}
}

func TestInAppAppendsAndPushes(t *testing.T) {
	db := &fakeDatabase{}
	hub := NewHub()
	user := uuid.New()
	subscriber, unsubscribe := hub.Subscribe(user)
	defer unsubscribe()

	sender := NewInApp(db, &Bus{hub: hub})
	task := testTask(0)
	task.UserId = user
	task.Channel = notify.ChannelInApp

	if err := sender.Send(context.Background(), task, "Заголовок", "Тело"); err != nil {
		t.Fatalf("доставка в приложение: %v", err)
	}
	if len(db.messages) != 1 {
		t.Fatalf("сообщений в ленте %d, ожидалось 1", len(db.messages))
	}

	select {
	case message := <-subscriber:
		if message.Title != "Заголовок" {
			t.Errorf("заголовок %q", message.Title)
		}
	case <-time.After(time.Second):
		t.Error("сообщение не дошло до активной сессии")
	}
}

func TestRunStopsOnContext(t *testing.T) {
	db := &fakeDatabase{}
	dispatcher := newTestDispatcher(t, db, &stubSender{channel: notify.ChannelInApp})
	dispatcher.Idle = 10 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- dispatcher.Run(ctx) }()

	cancel()
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("завершение с ошибкой: %v", err)
		}
	case <-time.After(time.Second):
		t.Error("воркер не завершился по отмене контекста")
	}
}

// TestRunProcessesQueueWithoutPause: пока очередь не пуста, проходы идут
// подряд — пауза между ними задержала бы доставку на ровном месте.
func TestRunProcessesQueueWithoutPause(t *testing.T) {
	db := &fakeDatabase{tasks: []Task{testTask(0), testTask(0)}}
	sender := &stubSender{channel: notify.ChannelTelegram}
	dispatcher := newTestDispatcher(t, db, sender)
	dispatcher.Idle = time.Hour

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- dispatcher.Run(ctx) }()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		db.mu.Lock()
		delivered := len(db.delivered)
		db.mu.Unlock()
		if delivered == 2 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Errorf("цикл вернул ошибку: %v", err)
	}

	db.mu.Lock()
	defer db.mu.Unlock()
	if len(db.delivered) != 2 {
		t.Errorf("доставлено %d заданий, ожидалось 2", len(db.delivered))
	}
}

// claimFailingDatabase отвечает отказом на выборку заданий: сбой прохода
// не должен останавливать разбор очереди.
type claimFailingDatabase struct {
	Database
	calls atomic.Int32
}

func (c *claimFailingDatabase) Claim(context.Context, int, time.Duration) ([]Task, error) {
	c.calls.Add(1)
	return nil, errors.New("connection refused")
}

func TestRunSurvivesClaimFailure(t *testing.T) {
	db := &claimFailingDatabase{}
	dispatcher := newTestDispatcher(t, db)
	dispatcher.Idle = time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	if err := dispatcher.Run(ctx); err != nil {
		t.Errorf("цикл вернул ошибку вместо остановки по контексту: %v", err)
	}
	if db.calls.Load() < 2 {
		t.Errorf("проходов %d: сбой остановил разбор очереди", db.calls.Load())
	}
}
