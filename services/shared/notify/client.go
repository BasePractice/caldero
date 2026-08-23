package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/google/uuid"
)

// publishTimeout ограничивает вызов сервиса оповещений. Публикация
// не должна задерживать бизнес-операцию: лучше не отправить оповещение,
// чем заставить пользователя ждать чужой сервис.
const publishTimeout = 3 * time.Second

// Client публикует события в сервис оповещений.
//
// Ошибка публикации — не повод отменять бизнес-операцию: подарок остаётся
// зарезервированным, даже если оповещение не ушло. Вызывающий обязан
// залогировать ошибку и продолжить.
type Client struct {
	endpoint string
	// serviceId — от чьего имени сервис публикует события. Оповещение
	// чужому пользователю требует роли оператора, и внутренние вызовы
	// идут с ней: границу держит то, что порты сервисов не опубликованы.
	serviceId uuid.UUID
	client    *http.Client
}

// NewClient создаёт клиента. Пустой адрес означает, что оповещения
// выключены: Publish в этом случае ничего не делает.
func NewClient(endpoint string, serviceId uuid.UUID) *Client {
	return &Client{
		endpoint:  endpoint,
		serviceId: serviceId,
		client:    &http.Client{Timeout: publishTimeout},
	}
}

// Enabled сообщает, настроен ли сервис оповещений.
func (c *Client) Enabled() bool {
	return c != nil && c.endpoint != "" && c.serviceId != uuid.Nil
}

// Publish отправляет событие.
func (c *Client) Publish(ctx context.Context, event PublishEvent) error {
	if !c.Enabled() {
		return nil
	}
	if err := event.Validate(); err != nil {
		return fmt.Errorf("invalid event: %w", err)
	}

	body, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("encoding event: %w", err)
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.endpoint+"/notify/events", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("creating publish request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Authorized-Id", c.serviceId.String())
	request.Header.Set("X-Roles", "operator")

	response, err := c.client.Do(request)
	if err != nil {
		return fmt.Errorf("publishing event %s: %w", event.Type, err)
	}
	defer func() {
		// Тело дочитывается ниже; здесь остаётся только закрыть.
		_ = response.Body.Close()
	}()

	if response.StatusCode != http.StatusAccepted {
		// Тело читается с ограничением: его размер задаёт чужой сервис.
		message, _ := io.ReadAll(io.LimitReader(response.Body, 1<<10))
		return fmt.Errorf("publishing event %s: %s: %s",
			event.Type, response.Status, bytes.TrimSpace(message))
	}
	return nil
}
