package main

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// TestRegisterQueueDepth: растущая очередь — первый признак того, что канал
// не работает, и значение обязано читаться на момент сбора метрик,
// а не на момент регистрации.
func TestRegisterQueueDepth(t *testing.T) {
	depth := int64(0)
	registerQueueDepth(func() int64 { return depth })
	// -1 означает, что подсчёт не удался: значение осмысленное и обязано
	// доезжать до метрики как есть.
	depth = -1

	expected := `
# HELP wish_notification_queue_depth Заданий в очереди доставки оповещений
# TYPE wish_notification_queue_depth gauge
wish_notification_queue_depth{service="notify"} -1
`
	if err := testutil.GatherAndCompare(prometheus.DefaultGatherer,
		strings.NewReader(expected), "wish_notification_queue_depth"); err != nil {
		t.Error(err)
	}
}
