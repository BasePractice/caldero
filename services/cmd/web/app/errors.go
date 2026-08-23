//go:build js && wasm

package main

import (
	"errors"
	"syscall/js"
)

// errText — ошибка с текстом для человека. Подробности реализации
// пользователю ничего не объясняют, а в браузере их некому читать.
func errText(text string) error {
	return errors.New(text)
}

func jsGlobalHistory() js.Value {
	return js.Global().Get("history")
}
