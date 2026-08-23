package services

import (
	"fmt"
	"net"
	"time"
)

// healthcheckTimeout — проба должна отвечать быстро или не отвечать вовсе.
const healthcheckTimeout = 2 * time.Second

// Healthcheck проверяет, что сервис принимает соединения. Проба живёт внутри
// самого бинарника, потому что образ собран из scratch: ни оболочки,
// ни curl там нет, и запустить внешнюю проверку нечем.
func Healthcheck(address string) error {
	conn, err := net.DialTimeout("tcp", address, healthcheckTimeout)
	if err != nil {
		return fmt.Errorf("connecting to %s: %w", address, err)
	}
	// Соединение установлено — этого достаточно; ошибка закрытия ни на что
	// не влияет.
	_ = conn.Close()
	return nil
}
