package caldron

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"wish/services/shared/credit"
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

// stream — детерминированный поток случайных чисел из зерна.
//
// HMAC-SHA256 с меткой и счётчиком: два розыгрыша от одного зерна
// (победитель и набор подарков) не должны использовать одни и те же байты,
// иначе они окажутся связаны.
type stream struct {
	seed    []byte
	label   string
	counter uint64
	buffer  []byte
}

func newStream(seed []byte, label string) *stream {
	return &stream{seed: seed, label: label}
}

func (s *stream) next() uint64 {
	if len(s.buffer) < 8 {
		mac := hmac.New(sha256.New, s.seed)
		// hash.Hash по контракту не возвращает ошибку записи.
		_, _ = mac.Write([]byte(s.label))
		var counter [8]byte
		binary.BigEndian.PutUint64(counter[:], s.counter)
		_, _ = mac.Write(counter[:])
		s.counter++
		s.buffer = mac.Sum(nil)
	}
	value := binary.BigEndian.Uint64(s.buffer[:8])
	s.buffer = s.buffer[8:]
	return value
}

// index возвращает равномерно распределённое число от 0 до n-1.
//
// Простой остаток от деления смещает распределение: при n, не делящем
// 2^64 нацело, младшие значения выпадают чаще. Значения из «хвоста»
// отбрасываются — в розыгрыше с деньгами перекос недопустим.
func (s *stream) index(n int) int {
	if n <= 1 {
		return 0
	}
	limit := ^uint64(0) - (^uint64(0) % uint64(n)) - 1
	for {
		value := s.next()
		if value <= limit {
			return int(value % uint64(n))
		}
	}
}

// SelectWinner выбирает победителя.
//
// Порядок участников задаёт результат, поэтому он фиксируется сортировкой
// по идентификатору: иначе один и тот же seed давал бы разных победителей
// в зависимости от того, как база вернула строки.
func SelectWinner(seed []byte, participants []uuid.UUID) (uuid.UUID, error) {
	if len(participants) == 0 {
		return uuid.Nil, ErrNoParticipants
	}

	ordered := sortedIds(participants)
	return ordered[newStream(seed, "winner").index(len(ordered))], nil
}

// SelectGifts выбирает набор подарков победителя в пределах суммы котла.
//
// Правило описано явно, потому что от него зависит, что человек получит:
// список перемешивается детерминированно, затем подарки берутся по порядку,
// пока очередной помещается в остаток бюджета. Пропуск слишком дорогого
// подарка не прекращает отбор — иначе один дорогой элемент в начале
// оставлял бы победителя почти ни с чем.
func SelectGifts(seed []byte, gifts []Gift, budget credit.Amount) ([]Gift, credit.Amount) {
	if len(gifts) == 0 || budget <= 0 {
		return []Gift{}, 0
	}

	shuffled := shuffle(seed, gifts)
	selected := make([]Gift, 0, len(shuffled))
	var total credit.Amount
	for _, gift := range shuffled {
		if gift.Price <= 0 || total+gift.Price > budget {
			continue
		}
		selected = append(selected, gift)
		total += gift.Price
	}
	return selected, total
}

// shuffle перемешивает список детерминированно (Фишер — Йетс).
func shuffle(seed []byte, gifts []Gift) []Gift {
	shuffled := make([]Gift, len(gifts))
	copy(shuffled, sortedGifts(gifts))

	source := newStream(seed, "gifts")
	for i := len(shuffled) - 1; i > 0; i-- {
		j := source.index(i + 1)
		shuffled[i], shuffled[j] = shuffled[j], shuffled[i]
	}
	return shuffled
}

// sortedIds и sortedGifts задают исходный порядок. Розыгрыш обязан быть
// воспроизводимым, а порядок строк из базы такой гарантии не даёт.
func sortedIds(ids []uuid.UUID) []uuid.UUID {
	ordered := make([]uuid.UUID, len(ids))
	copy(ordered, ids)
	for i := 1; i < len(ordered); i++ {
		for j := i; j > 0 && ordered[j].String() < ordered[j-1].String(); j-- {
			ordered[j], ordered[j-1] = ordered[j-1], ordered[j]
		}
	}
	return ordered
}

func sortedGifts(gifts []Gift) []Gift {
	ordered := make([]Gift, len(gifts))
	copy(ordered, gifts)
	for i := 1; i < len(ordered); i++ {
		for j := i; j > 0 && giftKey(ordered[j]) < giftKey(ordered[j-1]); j-- {
			ordered[j], ordered[j-1] = ordered[j-1], ordered[j]
		}
	}
	return ordered
}

func giftKey(gift Gift) string {
	return gift.Provider + ":" + gift.ProductId
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
