package caldron

import (
	"bytes"
	"errors"
	"testing"

	"github.com/google/uuid"

	"wish/services/shared/credit"
)

// fixedSeed — зерно из теста, а не из crypto/rand: розыгрыш обязан быть
// воспроизводимым, иначе проверить его нечем.
var fixedSeed = bytes.Repeat([]byte{0x2a}, SeedSize)

func testParticipants(n int) []uuid.UUID {
	participants := make([]uuid.UUID, n)
	for i := range participants {
		// Идентификаторы фиксированы: результат зависит от порядка,
		// и случайные значения сделали бы тест невоспроизводимым.
		participants[i] = uuid.MustParse(
			"00000000-0000-0000-0000-00000000000" + string(rune('1'+i)))
	}
	return participants
}

func TestDrawIsReproducible(t *testing.T) {
	participants := testParticipants(5)

	winner, err := SelectWinner(fixedSeed, participants)
	if err != nil {
		t.Fatalf("выбор победителя: %v", err)
	}
	for range 10 {
		again, err := SelectWinner(fixedSeed, participants)
		if err != nil {
			t.Fatalf("повторный выбор: %v", err)
		}
		if again != winner {
			t.Fatalf("один seed дал разных победителей: %s и %s", winner, again)
		}
	}

	t.Run("порядок участников не влияет на исход", func(t *testing.T) {
		reversed := make([]uuid.UUID, len(participants))
		for i, participant := range participants {
			reversed[len(participants)-1-i] = participant
		}
		shuffledWinner, err := SelectWinner(fixedSeed, reversed)
		if err != nil {
			t.Fatalf("выбор победителя: %v", err)
		}
		// Иначе исход зависел бы от того, как база вернула строки.
		if shuffledWinner != winner {
			t.Errorf("порядок изменил победителя: %s и %s", winner, shuffledWinner)
		}
	})

	t.Run("другое зерно даёт другой исход", func(t *testing.T) {
		other := bytes.Repeat([]byte{0x7f}, SeedSize)
		different := 0
		for i := range 20 {
			other[0] = byte(i)
			candidate, err := SelectWinner(other, participants)
			if err != nil {
				t.Fatalf("выбор победителя: %v", err)
			}
			if candidate != winner {
				different++
			}
		}
		if different == 0 {
			t.Error("двадцать разных зёрен дали одного и того же победителя")
		}
	})
}

func TestSelectWinnerWithoutParticipants(t *testing.T) {
	if _, err := SelectWinner(fixedSeed, nil); !errors.Is(err, ErrNoParticipants) {
		t.Errorf("получено %v, ожидалась %v", err, ErrNoParticipants)
	}
}

// TestWinnerDistribution проверяет, что розыгрыш не перекошен: при простом
// остатке от деления младшие участники выпадали бы чаще.
func TestWinnerDistribution(t *testing.T) {
	participants := testParticipants(3)
	counts := make(map[uuid.UUID]int, len(participants))

	const rounds = 6000
	seed := make([]byte, SeedSize)
	for i := range rounds {
		seed[0] = byte(i)
		seed[1] = byte(i >> 8)
		seed[2] = byte(i >> 16)
		winner, err := SelectWinner(seed, participants)
		if err != nil {
			t.Fatalf("выбор победителя: %v", err)
		}
		counts[winner]++
	}

	expected := rounds / len(participants)
	for participant, count := range counts {
		// Границы широкие: тест ловит перекос, а не проверяет качество
		// хеша. Отклонение больше трети говорит об ошибке в отборе.
		if count < expected*2/3 || count > expected*4/3 {
			t.Errorf("участник %s выиграл %d раз из %d, ожидалось около %d",
				participant, count, rounds, expected)
		}
	}
	if len(counts) != len(participants) {
		t.Errorf("выигрывали только %d участников из %d", len(counts), len(participants))
	}
}

func TestCommitment(t *testing.T) {
	commitment := Commit(fixedSeed)
	if len(commitment) != 64 {
		t.Fatalf("длина обязательства %d, ожидалось 64", len(commitment))
	}
	if !VerifyCommitment(fixedSeed, commitment) {
		t.Error("собственное зерно не прошло проверку")
	}

	t.Run("подменённое зерно не проходит", func(t *testing.T) {
		tampered := bytes.Clone(fixedSeed)
		tampered[0] ^= 0xff
		if VerifyCommitment(tampered, commitment) {
			t.Error("подменённое зерно прошло проверку")
		}
	})

	t.Run("испорченное обязательство не проходит", func(t *testing.T) {
		if VerifyCommitment(fixedSeed, "не хеш") {
			t.Error("испорченное обязательство прошло проверку")
		}
	})
}

func testGifts(prices ...credit.Amount) []Gift {
	gifts := make([]Gift, len(prices))
	for i, price := range prices {
		gifts[i] = Gift{
			Provider:  "STUB",
			ProductId: string(rune('a' + i)),
			Title:     "Подарок " + string(rune('a'+i)),
			Price:     price,
		}
	}
	return gifts
}

func TestSelectGiftsFitsBudget(t *testing.T) {
	// Пример из README: котёл на 10 000, подарки 3400, 1600 и 5000.
	gifts := testGifts(3_400_00, 1_600_00, 5_000_00)
	budget := credit.Amount(10_000_00)

	selected, total := SelectGifts(fixedSeed, gifts, budget)
	if len(selected) != 3 {
		t.Fatalf("выбрано %d подарков, ожидалось 3", len(selected))
	}
	if total != budget {
		t.Errorf("стоимость набора %s, ожидалось %s", total, budget)
	}
}

func TestSelectGiftsNeverExceedsBudget(t *testing.T) {
	gifts := testGifts(3_400_00, 1_600_00, 5_000_00, 900_00, 4_200_00)
	budget := credit.Amount(6_000_00)

	seed := make([]byte, SeedSize)
	for i := range 200 {
		seed[0] = byte(i)
		selected, total := SelectGifts(seed, gifts, budget)
		if total > budget {
			t.Fatalf("набор дороже котла: %s при бюджете %s", total, budget)
		}
		var sum credit.Amount
		for _, gift := range selected {
			sum += gift.Price
		}
		if sum != total {
			t.Fatalf("итог %s не совпал с суммой набора %s", total, sum)
		}
	}
}

// TestSelectGiftsSkipsTooExpensive фиксирует правило отбора: слишком
// дорогой подарок пропускается, но отбор продолжается — иначе один
// дорогой элемент в начале оставлял бы победителя почти ни с чем.
func TestSelectGiftsSkipsTooExpensive(t *testing.T) {
	gifts := testGifts(9_000_00, 1_000_00, 500_00)
	budget := credit.Amount(1_600_00)

	selected, total := SelectGifts(fixedSeed, gifts, budget)
	if total > budget {
		t.Fatalf("набор дороже котла: %s", total)
	}
	if len(selected) == 0 {
		t.Error("не выбрано ничего, хотя два подарка помещаются в бюджет")
	}
	for _, gift := range selected {
		if gift.Price > budget {
			t.Errorf("в набор попал подарок дороже котла: %s", gift.Price)
		}
	}
}

func TestSelectGiftsEdgeCases(t *testing.T) {
	if selected, total := SelectGifts(fixedSeed, nil, 1_000_00); len(selected) != 0 || total != 0 {
		t.Errorf("пустой список дал набор: %+v", selected)
	}
	if selected, _ := SelectGifts(fixedSeed, testGifts(100_00), 0); len(selected) != 0 {
		t.Errorf("при нулевом котле выбраны подарки: %+v", selected)
	}
}

func TestValidateGifts(t *testing.T) {
	tests := []struct {
		name    string
		gifts   []Gift
		budget  credit.Amount
		wantErr error
	}{
		{"список в пределах котла", testGifts(1_000_00, 2_000_00), 5_000_00, nil},
		{"ровно пять подарков", testGifts(100_00, 100_00, 100_00, 100_00, 100_00), 1_000_00, nil},
		{"шесть подарков", testGifts(100_00, 100_00, 100_00, 100_00, 100_00, 100_00), 1_000_00, ErrTooManyGifts},
		{"дороже котла", testGifts(3_000_00, 3_000_00), 5_000_00, ErrGiftsTooExpensive},
		{"пустой список", nil, 5_000_00, nil},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := ValidateGifts(test.gifts, test.budget)
			switch {
			case test.wantErr == nil && err != nil:
				t.Errorf("список отклонён: %v", err)
			case test.wantErr != nil && !errors.Is(err, test.wantErr):
				t.Errorf("получено %v, ожидалась %v", err, test.wantErr)
			}
		})
	}

	t.Run("подарок без цены", func(t *testing.T) {
		if err := ValidateGifts([]Gift{{Title: "Без цены"}}, 1_000_00); err == nil {
			t.Error("подарок без цены принят")
		}
	})
}

func TestNewSeed(t *testing.T) {
	first, err := NewSeed()
	if err != nil {
		t.Fatalf("генерация зерна: %v", err)
	}
	if len(first) != SeedSize {
		t.Errorf("длина зерна %d, ожидалось %d", len(first), SeedSize)
	}

	second, err := NewSeed()
	if err != nil {
		t.Fatalf("генерация зерна: %v", err)
	}
	if bytes.Equal(first, second) {
		t.Error("два зерна совпали: источник случайности не работает")
	}
}
