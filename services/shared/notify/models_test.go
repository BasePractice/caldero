package notify

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestEventTypeValid(t *testing.T) {
	for _, known := range EventTypes() {
		t.Run(string(known), func(t *testing.T) {
			if !known.Valid() {
				t.Error("перечисленный тип события считается неизвестным")
			}
		})
	}

	for _, unknown := range []EventType{"", "WHATEVER", "wishlist_item_reserved"} {
		t.Run("неизвестный "+string(unknown), func(t *testing.T) {
			if unknown.Valid() {
				t.Errorf("тип %q принят как известный", unknown)
			}
		})
	}
}

func TestChannelValid(t *testing.T) {
	for _, known := range Channels() {
		t.Run(string(known), func(t *testing.T) {
			if !known.Valid() {
				t.Error("перечисленный канал считается неизвестным")
			}
		})
	}

	for _, unknown := range []Channel{"", "SMS", "in_app"} {
		t.Run("неизвестный "+string(unknown), func(t *testing.T) {
			if unknown.Valid() {
				t.Errorf("канал %q принят как известный", unknown)
			}
		})
	}
}

// TestMessengersAreChannels фиксирует, что каналы через бота — часть общего
// списка: разойдись эти списки, доставка ушла бы в канал, которого нет
// в настройках.
func TestMessengersAreChannels(t *testing.T) {
	for _, messenger := range Messengers() {
		if !messenger.Valid() {
			t.Errorf("канал бота %q отсутствует в списке каналов", messenger)
		}
	}
}

func TestPublishEventValidate(t *testing.T) {
	tests := []struct {
		name    string
		event   PublishEvent
		wantErr bool
	}{
		{
			name:  "минимальное событие",
			event: PublishEvent{UserId: uuid.New(), Type: EventPaymentSettled},
		},
		{
			name: "событие с подстановками и ключом повтора",
			event: PublishEvent{
				UserId:   uuid.New(),
				Type:     EventWishlistItemGifted,
				Payload:  map[string]string{"item": "чайник"},
				DedupKey: "wishlist-item-42",
			},
		},
		{
			name:    "без получателя",
			event:   PublishEvent{Type: EventPaymentSettled},
			wantErr: true,
		},
		{
			name:    "неизвестный тип события",
			event:   PublishEvent{UserId: uuid.New(), Type: "WHATEVER"},
			wantErr: true,
		},
		{
			name: "подстановок больше предела",
			event: PublishEvent{
				UserId:  uuid.New(),
				Type:    EventPaymentSettled,
				Payload: payload(MaxPayloadEntries + 1),
			},
			wantErr: true,
		},
		{
			name: "ровно предельное число подстановок",
			event: PublishEvent{
				UserId:  uuid.New(),
				Type:    EventPaymentSettled,
				Payload: payload(MaxPayloadEntries),
			},
		},
		{
			name: "пустой ключ подстановки",
			event: PublishEvent{
				UserId:  uuid.New(),
				Type:    EventPaymentSettled,
				Payload: map[string]string{"": "значение"},
			},
			wantErr: true,
		},
		{
			name: "слишком длинное значение подстановки",
			event: PublishEvent{
				UserId:  uuid.New(),
				Type:    EventPaymentSettled,
				Payload: map[string]string{"item": strings.Repeat("я", MaxPayloadValue+1)},
			},
			wantErr: true,
		},
		{
			name: "слишком длинный ключ повтора",
			event: PublishEvent{
				UserId:   uuid.New(),
				Type:     EventPaymentSettled,
				DedupKey: strings.Repeat("k", 129),
			},
			wantErr: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.event.Validate()
			if test.wantErr && err == nil {
				t.Error("событие принято, ожидался отказ")
			}
			if !test.wantErr && err != nil {
				t.Errorf("событие отклонено: %v", err)
			}
		})
	}
}

func payload(entries int) map[string]string {
	result := make(map[string]string, entries)
	for i := range entries {
		result[string(rune('a'+i%26))+strings.Repeat("x", i/26+1)] = "значение"
	}
	return result
}

func TestPublishEventString(t *testing.T) {
	event := PublishEvent{UserId: uuid.New(), Type: EventCaldronDrawResult}

	got := event.String()
	for _, want := range []string{event.UserId.String(), string(EventCaldronDrawResult)} {
		if !strings.Contains(got, want) {
			t.Errorf("в %q нет %q", got, want)
		}
	}
}

func TestPreferenceValidate(t *testing.T) {
	tests := []struct {
		name       string
		preference Preference
		wantErr    bool
	}{
		{
			name:       "известные тип и канал",
			preference: Preference{Type: EventPaymentSettled, Channel: ChannelInApp, Enabled: true},
		},
		{
			name:       "неизвестный тип события",
			preference: Preference{Type: "WHATEVER", Channel: ChannelInApp},
			wantErr:    true,
		},
		{
			name:       "неизвестный канал",
			preference: Preference{Type: EventPaymentSettled, Channel: "SMS"},
			wantErr:    true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.preference.Validate()
			if test.wantErr && err == nil {
				t.Error("настройка принята, ожидался отказ")
			}
			if !test.wantErr && err != nil {
				t.Errorf("настройка отклонена: %v", err)
			}
		})
	}
}

// TestDefaultEnabled фиксирует главное правило умолчаний: код подтверждения
// не уходит в мессенджер, пока привязка не подтверждена.
func TestDefaultEnabled(t *testing.T) {
	tests := []struct {
		eventType EventType
		channel   Channel
		want      bool
	}{
		{EventConfirmationCode, ChannelTelegram, false},
		{EventConfirmationCode, ChannelMax, false},
		{EventConfirmationCode, ChannelEmail, true},
		{EventConfirmationCode, ChannelInApp, true},
		{EventConfirmationLink, ChannelTelegram, false},
		{EventConfirmationLink, ChannelMax, false},
		{EventConfirmationLink, ChannelEmail, true},
		{EventPaymentSettled, ChannelTelegram, true},
		{EventCaldronDrawResult, ChannelInApp, true},
	}

	for _, test := range tests {
		t.Run(string(test.eventType)+"/"+string(test.channel), func(t *testing.T) {
			if got := DefaultEnabled(test.eventType, test.channel); got != test.want {
				t.Errorf("получено %v, ожидалось %v", got, test.want)
			}
		})
	}
}
