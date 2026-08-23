package services

import (
	"log/slog"
	"runtime/debug"
)

// Значения подставляются при сборке через -ldflags -X. Без них непонятно,
// какая именно сборка работает в окружении, и разбор инцидента начинается
// с выяснения этого.
var (
	Version  = "dev"
	Revision = ""
)

// BuildInfo возвращает версию и ревизию сборки. Если ревизия не передана
// через ldflags, она берётся из данных, которые Go встраивает сам при
// сборке из git-репозитория.
func BuildInfo() (version, revision string) {
	version, revision = Version, Revision
	if revision != "" {
		return version, revision
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return version, revision
	}
	for _, setting := range info.Settings {
		if setting.Key == "vcs.revision" {
			return version, setting.Value
		}
	}
	return version, revision
}

func logBuildInfo(service string) {
	version, revision := BuildInfo()
	attrs := []any{slog.String("service", service), slog.String("version", version)}
	if revision != "" {
		attrs = append(attrs, slog.String("revision", revision))
	}
	slog.Info("Service starting", attrs...)
}
