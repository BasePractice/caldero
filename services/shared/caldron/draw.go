package caldron

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/google/uuid"

	"wish/services/shared/credit"
	"wish/services/shared/pick"
)

// SeedSize — размер случайного зерна розыгрыша.
const SeedSize = 32

// MaxGifts — сколько подарков участник вносит в свой список. Значение
// из README: список из пяти элементов.
const MaxGifts = 5

// Ошибки розыгрыша.
var (
	// ErrNoParticipants — разыгрывать не между кем.
	ErrNoParticipants = errors.New("no participants to draw from")
	// ErrAlreadyDrawn — розыгрыш уже состоялся. Результат неизменяем:
	// иначе организатор мог бы перекручивать его до нужного исхода.
	ErrAlreadyDrawn = errors.New("caldron has already been drawn")
	// ErrGiftsTooExpensive — список подарков дороже суммы котла.
	ErrGiftsTooExpensive = errors.New("gifts cost more than the caldron holds")
	// ErrTooManyGifts — в списке больше подарков, чем разрешено.
	ErrTooManyGifts = errors.New("too many gifts in the list")
)

// Gift — подарок из списка участника. Цена — снимок на момент добавления;
// на площадке она меняется, поэтому перед розыгрышем список сверяется заново.
type Gift struct {
	CaldronId uuid.UUID     `json:"caldron_id"`
	UserId    uuid.UUID     `json:"user_id"`
	Provider  string        `json:"provider"`
	ProductId string        `json:"product_id"`
	Title     string        `json:"title"`
	URL       string        `json:"url,omitempty"`
	Price     credit.Amount `json:"price"`
	PriceAt   time.Time     `json:"price_at"`
	CreatedAt time.Time     `json:"created_at"`
}

// Draw — результат розыгрыша. Хранится неизменяемым: он объясняет,
// почему победил именно этот участник, и переписывать его нельзя.
type Draw struct {
	CaldronId uuid.UUID `json:"caldron_id"`
	// Commitment публикуется до розыгрыша, Seed раскрывается после.
	// Вместе они позволяют участнику пересчитать результат самому.
	Commitment string    `json:"commitment"`
	Seed       string    `json:"seed,omitempty"`
	WinnerId   uuid.UUID `json:"winner_id"`
	// Gifts — набор, выпавший победителю, Total — его стоимость.
	Gifts []Gift        `json:"gifts"`
	Total credit.Amount `json:"total"`
	// Payout — остаток суммы котла сверх стоимости подарков.
	Payout    credit.Amount `json:"payout"`
	CreatedAt time.Time     `json:"created_at"`
}

// NewSeed выдаёт зерно розыгрыша.
//
// Только crypto/rand: math/rand выдаёт предсказуемую последовательность,
// и, зная момент запуска, исход розыгрыша с деньгами можно вычислить
// заранее. Для розыгрыша это не «менее качественная случайность»,
// а отсутствие случайности вообще.
func NewSeed() ([]byte, error) {
	seed := make([]byte, SeedSize)
	if _, err := rand.Read(seed); err != nil {
		return nil, fmt.Errorf("generating draw seed: %w", err)
	}
	return seed, nil
}

// Commit возвращает обязательство — хеш зерна.
//
// Обязательство публикуется до розыгрыша, зерно — после. Участник
// проверяет, что sha256(seed) совпадает с опубликованным обязательством,
// и пересчитывает результат сам. Без этого механизм розыгрыша неотличим
// от произвольного выбора организатором.
func Commit(seed []byte) string {
	sum := sha256.Sum256(seed)
	return hex.EncodeToString(sum[:])
}

// VerifyCommitment проверяет, что зерно соответствует обязательству.
func VerifyCommitment(seed []byte, commitment string) bool {
	// hmac.Equal, а не ==: сравнение с ранним выходом даёт лишний канал
	// для подбора.
	expected, err := hex.DecodeString(commitment)
	if err != nil {
		return false
	}
	actual, err := hex.DecodeString(Commit(seed))
	if err != nil {
		return false
	}
	return hmac.Equal(expected, actual)
}

// SelectWinner выбирает победителя.
//
// Порядок участников задаёт результат, поэтому он фиксируется сортировкой
// по идентификатору: иначе одно и то же зерно давало бы разных победителей
// в зависимости от того, как база вернула строки.
func SelectWinner(seed []byte, participants []uuid.UUID) (uuid.UUID, error) {
	if len(participants) == 0 {
		return uuid.Nil, ErrNoParticipants
	}

	ordered := make([]uuid.UUID, len(participants))
	copy(ordered, participants)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].String() < ordered[j].String() })

	return ordered[pick.NewStream(seed, "winner").Index(len(ordered))], nil
}

// SelectGifts выбирает набор подарков победителя в пределах суммы котла.
// Правило отбора общее для всей системы — см. pick.Within.
func SelectGifts(seed []byte, gifts []Gift, budget credit.Amount) ([]Gift, credit.Amount) {
	return pick.Within(seed, "gifts", gifts,
		func(gift Gift) string { return gift.Provider + ":" + gift.ProductId },
		func(gift Gift) credit.Amount { return gift.Price },
		budget)
}

// ValidateGifts проверяет список подарков участника.
//
// Проверка выполняется дважды: когда участник подтверждает список
// и ещё раз перед розыгрышем — цены на площадке меняются, и список,
// собранный неделю назад, мог перестать помещаться в котёл.
func ValidateGifts(gifts []Gift, budget credit.Amount) error {
	if len(gifts) > MaxGifts {
		return fmt.Errorf("%w: %d gifts, at most %d allowed", ErrTooManyGifts, len(gifts), MaxGifts)
	}

	var total credit.Amount
	for _, gift := range gifts {
		if gift.Price <= 0 {
			return fmt.Errorf("gift %q has no price", gift.Title)
		}
		total += gift.Price
	}
	if total > budget {
		return fmt.Errorf("%w: gifts cost %s, caldron holds %s", ErrGiftsTooExpensive, total, budget)
	}
	return nil
}
