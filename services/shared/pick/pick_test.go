package pick

import (
	"bytes"
	"testing"

	"wish/services/shared/credit"
)

type item struct {
	id    string
	price credit.Amount
}

func key(i item) string          { return i.id }
func price(i item) credit.Amount { return i.price }

var seed = bytes.Repeat([]byte{0x11}, 32)

func TestWithinFitsBudget(t *testing.T) {
	items := []item{{"a", 3_400_00}, {"b", 1_600_00}, {"c", 5_000_00}, {"d", 900_00}}
	budget := credit.Amount(6_000_00)

	source := make([]byte, 32)
	for i := range 200 {
		source[0] = byte(i)
		selected, total := Within(source, "test", items, key, price, budget)
		if total > budget {
			t.Fatalf("набор дороже бюджета: %s при %s", total, budget)
		}
		var sum credit.Amount
		for _, chosen := range selected {
			sum += chosen.price
		}
		if sum != total {
			t.Fatalf("итог %s не совпал с суммой набора %s", total, sum)
		}
	}
}

func TestWithinIsDeterministic(t *testing.T) {
	items := []item{{"a", 1_000_00}, {"b", 2_000_00}, {"c", 3_000_00}}

	first, total := Within(seed, "test", items, key, price, 4_000_00)
	for range 5 {
		again, againTotal := Within(seed, "test", items, key, price, 4_000_00)
		if len(again) != len(first) || againTotal != total {
			t.Fatalf("одно зерно дало разные наборы: %d и %d", len(first), len(again))
		}
		for i := range first {
			if again[i].id != first[i].id {
				t.Fatalf("порядок набора изменился: %s и %s", first[i].id, again[i].id)
			}
		}
	}

	t.Run("порядок входа не влияет на исход", func(t *testing.T) {
		reversed := make([]item, len(items))
		for i, value := range items {
			reversed[len(items)-1-i] = value
		}
		other, otherTotal := Within(seed, "test", reversed, key, price, 4_000_00)
		if otherTotal != total || len(other) != len(first) {
			t.Fatalf("порядок входа изменил исход: %s и %s", total, otherTotal)
		}
	})

}

// TestStreamLabelsAreIndependent: два отбора от одного зерна не должны
// использовать одни и те же байты, иначе они окажутся связаны.
func TestStreamLabelsAreIndependent(t *testing.T) {
	first := NewStream(seed, "winner")
	second := NewStream(seed, "gifts")

	same := 0
	const rounds = 50
	for range rounds {
		if first.Index(1000) == second.Index(1000) {
			same++
		}
	}
	if same > rounds/4 {
		t.Errorf("потоки с разными метками совпали %d раз из %d", same, rounds)
	}
}

func TestWithinSkipsTooExpensive(t *testing.T) {
	// Слишком дорогой элемент пропускается, но отбор продолжается.
	items := []item{{"expensive", 9_000_00}, {"cheap", 500_00}}
	selected, total := Within(seed, "test", items, key, price, 1_000_00)

	if len(selected) != 1 || selected[0].id != "cheap" {
		t.Errorf("выбрано %+v, ожидался только дешёвый элемент", selected)
	}
	if total != 500_00 {
		t.Errorf("итог %s, ожидалось %s", total, credit.Amount(500_00))
	}
}

func TestWithinEdgeCases(t *testing.T) {
	if selected, total := Within(seed, "test", nil, key, price, 1_000_00); len(selected) != 0 || total != 0 {
		t.Errorf("пустой вход дал набор: %+v", selected)
	}
	if selected, _ := Within(seed, "test", []item{{"a", 100}}, key, price, 0); len(selected) != 0 {
		t.Errorf("при нулевом бюджете выбраны элементы: %+v", selected)
	}
	if selected, _ := Within(seed, "test", []item{{"a", 0}}, key, price, 1_000_00); len(selected) != 0 {
		t.Errorf("элемент без цены попал в набор: %+v", selected)
	}
}

// TestIndexDistribution проверяет отсутствие перекоса: при простом остатке
// от деления младшие значения выпадали бы чаще.
func TestIndexDistribution(t *testing.T) {
	const buckets = 5
	const rounds = 10000

	counts := make([]int, buckets)
	source := NewStream(seed, "distribution")
	for range rounds {
		counts[source.Index(buckets)]++
	}

	expected := rounds / buckets
	for value, count := range counts {
		if count < expected*3/4 || count > expected*5/4 {
			t.Errorf("значение %d выпало %d раз из %d, ожидалось около %d",
				value, count, rounds, expected)
		}
	}
}

func TestIndexEdgeCases(t *testing.T) {
	source := NewStream(seed, "edge")
	for _, n := range []int{0, 1} {
		if got := source.Index(n); got != 0 {
			t.Errorf("Index(%d) = %d, ожидалось 0", n, got)
		}
	}
}
