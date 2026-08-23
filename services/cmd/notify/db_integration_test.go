//go:build integration

package main

import (
	"context"
	"sync"
	"testing"
	"time"

	"wish/services/shared/notify"
	"wish/services/testsupport"

	"github.com/google/uuid"
)

func newTestDatabase(t *testing.T) Database {
	t.Helper()
	db, err := NewDatabase(context.Background(), testsupport.Prepare(t, "notify"))
	if err != nil {
		t.Fatalf("не удалось открыть репозиторий: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestPublishDeduplicates(t *testing.T) {
	ctx := context.Background()
	db := newTestDatabase(t)
	user := uuid.New()

	event := notify.PublishEvent{
		UserId:   user,
		Type:     notify.EventCaldronStateChanged,
		Payload:  map[string]string{"caldron": "День рождения", "state": "готов"},
		DedupKey: "caldron-1-ready",
	}

	first, duplicate, err := db.Publish(ctx, event, []notify.Channel{notify.ChannelInApp})
	if err != nil {
		t.Fatalf("публикация: %v", err)
	}
	if duplicate {
		t.Error("первая публикация отмечена повтором")
	}

	// Публикующий сервис ретраит запрос, не зная, дошёл ли предыдущий.
	second, duplicate, err := db.Publish(ctx, event, []notify.Channel{notify.ChannelInApp})
	if err != nil {
		t.Fatalf("повторная публикация: %v", err)
	}
	if !duplicate {
		t.Error("повтор не распознан")
	}
	if first.Id != second.Id {
		t.Errorf("повтор создал второе событие: %s и %s", first.Id, second.Id)
	}

	tasks, err := db.Claim(ctx, 10, time.Minute)
	if err != nil {
		t.Fatalf("выборка заданий: %v", err)
	}
	if len(tasks) != 1 {
		t.Errorf("заданий на доставку %d, ожидалось 1", len(tasks))
	}
}

func TestPublishWithoutDedupKey(t *testing.T) {
	ctx := context.Background()
	db := newTestDatabase(t)
	user := uuid.New()

	event := notify.PublishEvent{
		UserId:  user,
		Type:    notify.EventWishlistItemReserved,
		Payload: map[string]string{"item": "Кофеварка"},
	}
	first, _, err := db.Publish(ctx, event, []notify.Channel{notify.ChannelInApp})
	if err != nil {
		t.Fatalf("публикация: %v", err)
	}
	second, duplicate, err := db.Publish(ctx, event, []notify.Channel{notify.ChannelInApp})
	if err != nil {
		t.Fatalf("вторая публикация: %v", err)
	}
	// Без ключа дедупликации событие считается новым: система не может
	// отличить повтор от второго такого же подарка.
	if duplicate || first.Id == second.Id {
		t.Error("события без ключа дедупликации схлопнулись в одно")
	}
}

// TestClaimSkipsLocked проверяет, что два воркера не берут одно задание.
func TestClaimSkipsLocked(t *testing.T) {
	ctx := context.Background()
	db := newTestDatabase(t)

	const events = 20
	for i := range events {
		if _, _, err := db.Publish(ctx, notify.PublishEvent{
			UserId:  uuid.New(),
			Type:    notify.EventWishlistItemReserved,
			Payload: map[string]string{"item": "Товар"},
		}, []notify.Channel{notify.ChannelInApp}); err != nil {
			t.Fatalf("публикация %d: %v", i, err)
		}
	}

	var (
		mu    sync.Mutex
		taken = make(map[uuid.UUID]int)
		wg    sync.WaitGroup
	)
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tasks, err := db.Claim(ctx, events, time.Minute)
			if err != nil {
				t.Errorf("выборка заданий: %v", err)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			for _, task := range tasks {
				taken[task.Id]++
			}
		}()
	}
	wg.Wait()

	for id, count := range taken {
		if count > 1 {
			t.Errorf("задание %s взято %d раз", id, count)
		}
	}
	if len(taken) != events {
		t.Errorf("взято заданий %d, ожидалось %d", len(taken), events)
	}
}

func TestClaimRespectsLease(t *testing.T) {
	ctx := context.Background()
	db := newTestDatabase(t)

	if _, _, err := db.Publish(ctx, notify.PublishEvent{
		UserId:  uuid.New(),
		Type:    notify.EventWishlistItemReserved,
		Payload: map[string]string{"item": "Товар"},
	}, []notify.Channel{notify.ChannelInApp}); err != nil {
		t.Fatalf("публикация: %v", err)
	}

	if tasks, err := db.Claim(ctx, 10, time.Hour); err != nil || len(tasks) != 1 {
		t.Fatalf("первая выборка: %d заданий, ошибка %v", len(tasks), err)
	}
	// Арендованное задание не должно попасть во вторую выборку, пока
	// первый воркер его обрабатывает.
	if tasks, err := db.Claim(ctx, 10, time.Hour); err != nil || len(tasks) != 0 {
		t.Errorf("арендованное задание выдано повторно: %d заданий, ошибка %v", len(tasks), err)
	}
}

func TestMessageCursor(t *testing.T) {
	ctx := context.Background()
	db := newTestDatabase(t)
	user := uuid.New()

	var last int64
	for i := range 3 {
		event, _, err := db.Publish(ctx, notify.PublishEvent{
			UserId:  user,
			Type:    notify.EventWishlistItemReserved,
			Payload: map[string]string{"item": "Товар"},
		}, []notify.Channel{notify.ChannelInApp})
		if err != nil {
			t.Fatalf("публикация %d: %v", i, err)
		}
		message, err := db.AppendMessage(ctx, Task{
			EventId: event.Id, UserId: user, Type: event.Type,
		}, "Заголовок", "Тело")
		if err != nil {
			t.Fatalf("добавление сообщения %d: %v", i, err)
		}
		if message.Seq <= last {
			t.Fatalf("номер сообщения не растёт: %d после %d", message.Seq, last)
		}
		last = message.Seq
	}

	messages, err := db.Messages(ctx, user, 0, 10)
	if err != nil {
		t.Fatalf("чтение ленты: %v", err)
	}
	if len(messages) != 3 {
		t.Fatalf("сообщений %d, ожидалось 3", len(messages))
	}

	tail, err := db.Messages(ctx, user, messages[1].Seq, 10)
	if err != nil {
		t.Fatalf("чтение по курсору: %v", err)
	}
	if len(tail) != 1 || tail[0].Seq != messages[2].Seq {
		t.Errorf("по курсору получено %d сообщений", len(tail))
	}
}

// TestAppendMessageIdempotent проверяет, что повторная доставка после сбоя
// не показывает пользователю дубль.
func TestAppendMessageIdempotent(t *testing.T) {
	ctx := context.Background()
	db := newTestDatabase(t)
	user := uuid.New()

	event, _, err := db.Publish(ctx, notify.PublishEvent{
		UserId:  user,
		Type:    notify.EventWishlistItemReserved,
		Payload: map[string]string{"item": "Товар"},
	}, []notify.Channel{notify.ChannelInApp})
	if err != nil {
		t.Fatalf("публикация: %v", err)
	}

	task := Task{EventId: event.Id, UserId: user, Type: event.Type}
	first, err := db.AppendMessage(ctx, task, "Заголовок", "Тело")
	if err != nil {
		t.Fatalf("первая доставка: %v", err)
	}
	second, err := db.AppendMessage(ctx, task, "Заголовок", "Тело")
	if err != nil {
		t.Fatalf("повторная доставка: %v", err)
	}
	if first.Id != second.Id {
		t.Errorf("повтор создал второе сообщение: %s и %s", first.Id, second.Id)
	}

	messages, err := db.Messages(ctx, user, 0, 10)
	if err != nil {
		t.Fatalf("чтение ленты: %v", err)
	}
	if len(messages) != 1 {
		t.Errorf("сообщений в ленте %d, ожидалось 1", len(messages))
	}
}

func TestPreferencesFilterChannels(t *testing.T) {
	ctx := context.Background()
	db := newTestDatabase(t)
	user := uuid.New()

	// По умолчанию код подтверждения в мессенджеры не уходит: до
	// подтверждения привязки бот может быть чужим. Почта — исключение:
	// код подтверждения адреса иначе доставить некуда.
	channels, err := db.EnabledChannels(ctx, user, notify.EventConfirmationCode)
	if err != nil {
		t.Fatalf("каналы по умолчанию: %v", err)
	}
	for _, channel := range channels {
		if channel == notify.ChannelTelegram || channel == notify.ChannelMax {
			t.Errorf("код подтверждения уходит в мессенджер %s", channel)
		}
	}
	if !containsChannel(channels, notify.ChannelInApp) || !containsChannel(channels, notify.ChannelEmail) {
		t.Errorf("каналы по умолчанию для кода подтверждения: %v", channels)
	}

	if err = db.SetPreference(ctx, user, notify.Preference{
		Type: notify.EventCaldronStateChanged, Channel: notify.ChannelTelegram, Enabled: false,
	}); err != nil {
		t.Fatalf("сохранение настройки: %v", err)
	}
	channels, err = db.EnabledChannels(ctx, user, notify.EventCaldronStateChanged)
	if err != nil {
		t.Fatalf("каналы после настройки: %v", err)
	}
	if containsChannel(channels, notify.ChannelTelegram) {
		t.Errorf("выключенный канал остался в списке: %v", channels)
	}
	if !containsChannel(channels, notify.ChannelInApp) {
		t.Errorf("настройка одного канала выключила остальные: %v", channels)
	}

	preferences, err := db.Preferences(ctx, user)
	if err != nil {
		t.Fatalf("чтение настроек: %v", err)
	}
	if want := len(notify.EventTypes()) * len(notify.Channels()); len(preferences) != want {
		t.Errorf("настроек %d, ожидалось %d", len(preferences), want)
	}
}

func TestMessengerBindingLifecycle(t *testing.T) {
	ctx := context.Background()
	db := newTestDatabase(t)
	user := uuid.New()
	telegram := NewMessenger(db, TelegramConfig("test-token", "", "wish_bot"))

	if _, err := db.MessengerBinding(ctx, notify.ChannelTelegram, user); err == nil {
		t.Error("привязка найдена до её создания")
	}

	code := "ABCD2345"
	if err := db.StartMessengerBinding(ctx, notify.ChannelTelegram, user, telegram.BindingCodeHash(code),
		time.Now().Add(time.Minute)); err != nil {
		t.Fatalf("начало привязки: %v", err)
	}

	t.Run("чужой код не подходит", func(t *testing.T) {
		if _, err := db.CompleteMessengerBinding(ctx, notify.ChannelTelegram, telegram.BindingCodeHash("WRONGCOD"), 1); err == nil {
			t.Error("привязка завершена чужим кодом")
		}
	})

	bound, err := db.CompleteMessengerBinding(ctx, notify.ChannelTelegram, telegram.BindingCodeHash(code), 4242)
	if err != nil {
		t.Fatalf("завершение привязки: %v", err)
	}
	if bound != user {
		t.Errorf("привязан пользователь %s, ожидался %s", bound, user)
	}

	binding, err := db.MessengerBinding(ctx, notify.ChannelTelegram, user)
	if err != nil {
		t.Fatalf("чтение привязки: %v", err)
	}
	if binding.ChatId != 4242 || binding.Blocked {
		t.Errorf("привязка: %+v", binding)
	}

	t.Run("код одноразовый", func(t *testing.T) {
		if _, err := db.CompleteMessengerBinding(ctx, notify.ChannelTelegram, telegram.BindingCodeHash(code), 9); err == nil {
			t.Error("код сработал повторно")
		}
	})

	t.Run("блокировка бота запоминается", func(t *testing.T) {
		if err := db.BlockMessenger(ctx, notify.ChannelTelegram, user); err != nil {
			t.Fatalf("отметка блокировки: %v", err)
		}
		binding, err := db.MessengerBinding(ctx, notify.ChannelTelegram, user)
		if err != nil {
			t.Fatalf("чтение привязки: %v", err)
		}
		if !binding.Blocked {
			t.Error("блокировка не сохранена")
		}
	})
}

func TestDeliveryStates(t *testing.T) {
	ctx := context.Background()
	db := newTestDatabase(t)

	publish := func(t *testing.T) Task {
		t.Helper()
		event, _, err := db.Publish(ctx, notify.PublishEvent{
			UserId:  uuid.New(),
			Type:    notify.EventWishlistItemReserved,
			Payload: map[string]string{"item": "Товар"},
		}, []notify.Channel{notify.ChannelInApp})
		if err != nil {
			t.Fatalf("публикация: %v", err)
		}
		tasks, err := db.Claim(ctx, 1, time.Millisecond)
		if err != nil || len(tasks) != 1 {
			t.Fatalf("выборка: %d заданий, ошибка %v", len(tasks), err)
		}
		_ = event
		return tasks[0]
	}

	t.Run("доставленное задание уходит из очереди", func(t *testing.T) {
		task := publish(t)
		if err := db.Delivered(ctx, task.Id); err != nil {
			t.Fatalf("отметка доставки: %v", err)
		}
		tasks, err := db.Claim(ctx, 10, time.Millisecond)
		if err != nil {
			t.Fatalf("выборка: %v", err)
		}
		for _, claimed := range tasks {
			if claimed.Id == task.Id {
				t.Error("доставленное задание выдано снова")
			}
		}
	})

	t.Run("повтор считает попытки", func(t *testing.T) {
		task := publish(t)
		if err := db.Retry(ctx, task.Id, 0, "канал недоступен"); err != nil {
			t.Fatalf("отсрочка: %v", err)
		}
		tasks, err := db.Claim(ctx, 10, time.Millisecond)
		if err != nil {
			t.Fatalf("выборка: %v", err)
		}
		var found bool
		for _, claimed := range tasks {
			if claimed.Id == task.Id {
				found = true
				if claimed.Attempts != 1 {
					t.Errorf("попыток %d, ожидалась 1", claimed.Attempts)
				}
			}
		}
		if !found {
			t.Error("отложенное задание не вернулось в очередь")
		}
	})

	t.Run("снятое задание не возвращается", func(t *testing.T) {
		task := publish(t)
		if err := db.Failed(ctx, task.Id, "бот заблокирован"); err != nil {
			t.Fatalf("снятие с доставки: %v", err)
		}
		tasks, err := db.Claim(ctx, 10, time.Millisecond)
		if err != nil {
			t.Fatalf("выборка: %v", err)
		}
		for _, claimed := range tasks {
			if claimed.Id == task.Id {
				t.Error("снятое задание выдано снова")
			}
		}
	})
}

// containsChannel — проверка вхождения: каналов стало четыре, и сравнивать
// список целиком в каждом тесте значит переписывать их при добавлении
// следующего.
func containsChannel(channels []notify.Channel, channel notify.Channel) bool {
	for _, candidate := range channels {
		if candidate == channel {
			return true
		}
	}
	return false
}

// TestQueueMetricsAndDeferral закрывает то, что видно только из метрик
// и расписания: длину очереди, счётчик доставленных за окно и отложенную
// попытку. Растущая очередь — первый признак сломанного канала.
func TestQueueMetricsAndDeferral(t *testing.T) {
	ctx := context.Background()
	db := newTestDatabase(t)
	user := uuid.New()

	if _, _, err := db.Publish(ctx, notify.PublishEvent{
		UserId: user, Type: notify.EventPaymentSettled,
	}, []notify.Channel{notify.ChannelInApp}); err != nil {
		t.Fatalf("публикация: %v", err)
	}

	pending, err := db.Unsettled(ctx)
	if err != nil {
		t.Fatalf("подсчёт очереди: %v", err)
	}
	if pending != 1 {
		t.Errorf("в очереди %d заданий, ожидалось одно", pending)
	}

	tasks, err := db.Claim(ctx, 10, time.Minute)
	if err != nil {
		t.Fatalf("выборка заданий: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("выбрано %d заданий, ожидалось одно", len(tasks))
	}

	t.Run("отложенное задание не выбирается сразу", func(t *testing.T) {
		if err := db.Defer(ctx, tasks[0].Id, time.Hour); err != nil {
			t.Fatalf("отсрочка: %v", err)
		}
		// Аренда предыдущей выборки истекает через минуту, но отсрочка
		// на час обязана держать задание вне выборки и после неё.
		again, err := db.Claim(ctx, 10, 0)
		if err != nil {
			t.Fatalf("выборка заданий: %v", err)
		}
		for _, task := range again {
			if task.Id == tasks[0].Id {
				t.Error("отложенное задание выбрано раньше срока")
			}
		}
	})

	t.Run("доставленные за окно считаются", func(t *testing.T) {
		before, err := db.SentSince(ctx, user, notify.ChannelInApp, time.Hour)
		if err != nil {
			t.Fatalf("подсчёт доставленных: %v", err)
		}
		if err := db.Delivered(ctx, tasks[0].Id); err != nil {
			t.Fatalf("отметка доставки: %v", err)
		}
		after, err := db.SentSince(ctx, user, notify.ChannelInApp, time.Hour)
		if err != nil {
			t.Fatalf("подсчёт доставленных: %v", err)
		}
		if after != before+1 {
			t.Errorf("доставленных %d, ожидалось %d", after, before+1)
		}

		// Окно ограничивает счёт: доставка минуту назад не попадает
		// в нулевое окно, иначе ограничение частоты не работало бы.
		narrow, err := db.SentSince(ctx, user, notify.ChannelInApp, 0)
		if err != nil {
			t.Fatalf("подсчёт доставленных: %v", err)
		}
		if narrow != 0 {
			t.Errorf("в нулевом окне насчитано %d доставок", narrow)
		}
	})

	t.Run("проба готовности и статистика пула", func(t *testing.T) {
		if err := db.Ping(ctx); err != nil {
			t.Errorf("проба готовности: %v", err)
		}
		if db.Stats().MaxOpenConnections == 0 {
			t.Error("статистика пула не заполнена")
		}
	})
}
