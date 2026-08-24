// Package pick — детерминированный случайный отбор.
//
// Пакет общий, потому что правило «выбрать случайный набор в пределах
// суммы» нужно и котлу подарков, и шопоголику. Две реализации одного
// правила разошлись бы при первой же правке, а объяснять пользователю
// пришлось бы обе.
package pick

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"sort"

	"wish/services/shared/credit"
)

// Stream — детерминированный поток случайных чисел из зерна.
//
// HMAC-SHA256 с меткой и счётчиком: два розыгрыша от одного зерна
// не должны использовать одни и те же байты, иначе они окажутся связаны.
type Stream struct {
	seed    []byte
	label   string
	counter uint64
	buffer  []byte
}

func NewStream(seed []byte, label string) *Stream {
	return &Stream{seed: seed, label: label}
}

func (s *Stream) next() uint64 {
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

// Index возвращает равномерно распределённое число от 0 до n-1.
//
// Простой остаток от деления смещает распределение: при n, не делящем
// 2^64 нацело, младшие значения выпадают чаще. Значения из «хвоста»
// отбрасываются — там, где выбор решает судьбу денег, перекос недопустим.
func (s *Stream) Index(n int) int {
	if n <= 1 {
		return 0
	}
	limit := ^uint64(0) - (^uint64(0) % uint64(n)) - 1
	for {
		value := s.next()
		if value <= limit {
			// Остаток строго меньше n, а n — положительный int,
			// поэтому перевод обратно в int всегда в диапазоне.
			//nolint:gosec // G115: остаток меньше n, переполнение невозможно
			return int(value % uint64(n))
		}
	}
}

// Shuffle перемешивает элементы детерминированно (Фишер — Йетс).
//
// Порядок исходного среза задаёт результат, поэтому он сначала
// приводится к каноническому: иначе одно и то же зерно давало бы разный
// исход в зависимости от того, как база вернула строки.
func Shuffle[T any](seed []byte, label string, items []T, key func(T) string) []T {
	ordered := make([]T, len(items))
	copy(ordered, items)
	sort.SliceStable(ordered, func(i, j int) bool { return key(ordered[i]) < key(ordered[j]) })

	source := NewStream(seed, label)
	for i := len(ordered) - 1; i > 0; i-- {
		j := source.Index(i + 1)
		ordered[i], ordered[j] = ordered[j], ordered[i]
	}
	return ordered
}

// Within выбирает случайный набор в пределах бюджета.
//
// Правило описано явно, потому что от него зависит, что человек получит:
// список перемешивается детерминированно, затем элементы берутся
// по порядку, пока очередной помещается в остаток бюджета. Слишком дорогой
// пропускается, но отбор продолжается — иначе один дорогой элемент
// в начале оставлял бы почти ни с чем.
//
// Это простейший вариант задачи о рюкзаке. Точное решение здесь не нужно
// и даже вредно: набор должен быть случайным, а не максимально плотно
// набитым, иначе при одном и том же списке результат всегда одинаков.
func Within[T any](
	seed []byte,
	label string,
	items []T,
	key func(T) string,
	price func(T) credit.Amount,
	budget credit.Amount,
) ([]T, credit.Amount) {
	if len(items) == 0 || budget <= 0 {
		return []T{}, 0
	}

	selected := make([]T, 0, len(items))
	var total credit.Amount
	for _, item := range Shuffle(seed, label, items, key) {
		cost := price(item)
		if cost <= 0 || total+cost > budget {
			continue
		}
		selected = append(selected, item)
		total += cost
	}
	return selected, total
}
