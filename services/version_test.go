package services

import (
	"net"
	"testing"
)

// TestBuildInfo фиксирует источник ревизии: она либо приходит через ldflags,
// либо берётся из данных сборки. Без ревизии разбор инцидента начинается
// с выяснения того, какая сборка работает.
func TestBuildInfo(t *testing.T) {
	previousVersion, previousRevision := Version, Revision
	t.Cleanup(func() { Version, Revision = previousVersion, previousRevision })

	t.Run("значения из ldflags", func(t *testing.T) {
		Version, Revision = "1.2.3", "deadbeef"

		version, revision := BuildInfo()
		if version != "1.2.3" || revision != "deadbeef" {
			t.Errorf("получено %s/%s, ожидалось 1.2.3/deadbeef", version, revision)
		}
	})

	t.Run("без ревизии из ldflags", func(t *testing.T) {
		Version, Revision = "dev", ""

		version, revision := BuildInfo()
		if version != "dev" {
			t.Errorf("версия %q, ожидалась dev", version)
		}
		// Ревизия либо взята из данных сборки, либо осталась пустой:
		// в тестовом бинарнике vcs.revision присутствует не всегда.
		if revision != "" && len(revision) < 7 {
			t.Errorf("ревизия %q не похожа на хеш коммита", revision)
		}
	})

	// logBuildInfo пишет в журнал и ничего не возвращает: проверяется, что
	// обе ветки — с ревизией и без — отрабатывают.
	Revision = "deadbeef"
	logBuildInfo("test")
	Revision = ""
	logBuildInfo("test")
}

// TestHealthcheck проверяет пробу, которая живёт внутри бинарника: образ
// собран из scratch, и запустить внешнюю проверку в нём нечем.
func TestHealthcheck(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("занять порт: %v", err)
	}
	defer func() { _ = listener.Close() }()

	if err := Healthcheck(listener.Addr().String()); err != nil {
		t.Errorf("проба не прошла: %v", err)
	}
}

func TestHealthcheckUnreachable(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("занять порт: %v", err)
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("освободить порт: %v", err)
	}

	if err := Healthcheck(addr); err == nil {
		t.Fatal("проба прошла на закрытом порту")
	}
}
