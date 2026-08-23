package main

import "github.com/prometheus/client_golang/prometheus"

// deliveries считает исходы доставки по каналам. Один счётчик отправок
// ничего не говорит: важно соотношение доставленного, отложенного
// и брошенного — по нему видно, что канал перестал работать.
var deliveries = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "wish_notification_deliveries_total",
	Help: "Исходы доставки оповещений",
}, []string{"channel", "outcome"})

func init() {
	prometheus.MustRegister(deliveries)
}

// registerQueueDepth публикует длину очереди доставки. Растущая очередь —
// первый признак того, что канал не работает, и заметить это нужно раньше,
// чем пожалуются пользователи. Значение -1 означает, что подсчёт не удался.
func registerQueueDepth(count func() int64) {
	prometheus.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name:        "wish_notification_queue_depth",
		Help:        "Заданий в очереди доставки оповещений",
		ConstLabels: prometheus.Labels{"service": "notify"},
	}, func() float64 { return float64(count()) }))
}
