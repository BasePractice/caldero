package main

import (
	"strings"
	"testing"

	"wish/services/shared/notify"
)

// payloads — подстановки, которые публикующий сервис обязан передать
// для каждого типа события. Тест держит их рядом с шаблонами: иначе
// расхождение обнаружится в момент доставки, а не при сборке.
var payloads = map[notify.EventType]map[string]string{
	notify.EventWishlistItemReserved: {"item": "Кофеварка"},
	notify.EventWishlistItemGifted:   {"item": "Кофеварка"},
	notify.EventCaldronMemberAdded:   {"caldron": "День рождения", "amount": "1500"},
	notify.EventCaldronStateChanged:  {"caldron": "День рождения", "state": "готов к розыгрышу"},
	notify.EventCaldronDrawResult:    {"caldron": "День рождения", "winner": "Пётр"},
	notify.EventPaymentSettled:       {"amount": "1500", "status": "успешно"},
	notify.EventConfirmationCode:     {"code": "123456", "minutes": "15"},
}

func TestTemplatesCoverAllEvents(t *testing.T) {
	templates, err := LoadTemplates()
	if err != nil {
		t.Fatalf("загрузка шаблонов: %v", err)
	}

	for _, eventType := range notify.EventTypes() {
		t.Run(string(eventType), func(t *testing.T) {
			payload, ok := payloads[eventType]
			if !ok {
				t.Fatalf("для события %s не описаны подстановки", eventType)
			}
			title, body, err := templates.Render(eventType, payload)
			if err != nil {
				t.Fatalf("рендер: %v", err)
			}
			if title == "" {
				t.Error("пустой заголовок")
			}
			if body == "" {
				t.Error("пустое тело")
			}
			for _, value := range payload {
				if !strings.Contains(title+body, value) {
					t.Errorf("подстановка %q не попала в сообщение: %s / %s", value, title, body)
				}
			}
		})
	}
}

// TestTemplateMissingKey проверяет главное свойство настройки missingkey:
// сообщение с дырой посреди фразы не отправляется вовсе.
func TestTemplateMissingKey(t *testing.T) {
	templates, err := LoadTemplates()
	if err != nil {
		t.Fatalf("загрузка шаблонов: %v", err)
	}

	if _, _, err = templates.Render(notify.EventConfirmationCode, map[string]string{}); err == nil {
		t.Error("шаблон отрендерился без обязательной подстановки")
	}
}
