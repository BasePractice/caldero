package marketplace

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"time"

	"wish/services/shared/credit"
)

// Stub — площадка для локальной разработки и тестов.
//
// Существует потому, что без неё разработка списка желаний упирается
// в доступ к чужому API. Карточка строится из идентификатора детерминированно:
// один и тот же идентификатор всегда даёт одну и ту же цену, иначе тесты
// становятся невоспроизводимыми.
type Stub struct {
	// Unavailable позволяет проверить поведение при недоступной площадке.
	Unavailable bool
	// OrderSupported отражает то, что оформление заказа от имени покупателя
	// доступно не на всякой площадке.
	OrderSupported bool
}

func (s *Stub) Provider() Provider {
	return ProviderStub
}

func (s *Stub) Product(_ context.Context, id string) (Product, error) {
	if s.Unavailable {
		return Product{}, ErrUnavailable
	}
	if id == "" {
		return Product{}, ErrNotFound
	}

	sum := sha256.Sum256([]byte(id))
	// Цена от 100 до 100 000 рублей в копейках.
	price := credit.Amount(binary.BigEndian.Uint32(sum[:4])%9_990_000 + 10_000)

	return Product{
		Provider:  ProviderStub,
		Id:        id,
		Title:     "Товар " + id,
		Price:     price,
		URL:       "https://example.invalid/product/" + id,
		InStock:   sum[4]%4 != 0,
		FetchedAt: time.Now(),
	}, nil
}

func (s *Stub) Order(_ context.Context, id string, _ string) (string, error) {
	if s.Unavailable {
		return "", ErrUnavailable
	}
	if !s.OrderSupported {
		return "", ErrUnsupported
	}
	return fmt.Sprintf("stub-order-%s", id), nil
}
