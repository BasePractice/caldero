//go:build js && wasm

package main

import (
	"html"
	"syscall/js"
)

// Работа с DOM без фреймворка: экранов немного, а фреймворк принёс бы
// собственный жизненный цикл, свою сборку и зависимость, которую пришлось
// бы сопровождать ради четырёх страниц.

var document = js.Global().Get("document")

// byId возвращает элемент по идентификатору.
func byId(id string) js.Value {
	return document.Call("getElementById", id)
}

// setHTML подставляет готовую разметку.
//
// Всё, что приходит из API, обязательно проходит через escape: подставлять
// ответ сервера в разметку как есть — это межсайтовый скриптинг, даже если
// сервер свой.
func setHTML(id, markup string) {
	byId(id).Set("innerHTML", markup)
}

// escape экранирует текст для вставки в разметку.
func escape(text string) string {
	return html.EscapeString(text)
}

// onClick вешает обработчик на элемент.
//
// js.Func освобождать нельзя, пока обработчик нужен: приложение живёт
// до закрытия вкладки, и функции живут вместе с ним.
func onClick(id string, handler func()) {
	element := byId(id)
	if element.IsNull() || element.IsUndefined() {
		return
	}
	element.Call("addEventListener", "click", js.FuncOf(func(js.Value, []js.Value) any {
		go handler()
		return nil
	}))
}

// value читает значение поля ввода.
func value(id string) string {
	element := byId(id)
	if element.IsNull() || element.IsUndefined() {
		return ""
	}
	return element.Get("value").String()
}

// status показывает строку состояния над экраном.
func status(text string) {
	setHTML("status", escape(text))
}

// fail показывает ошибку. Текст ошибки — это текст для человека,
// а не подробности реализации.
func fail(text string) {
	setHTML("status", `<span class="error">`+escape(text)+`</span>`)
}

// location даёт доступ к адресной строке.
func location() js.Value {
	return js.Global().Get("location")
}

// sessionValue читает значение из sessionStorage.
func sessionValue(key string) string {
	value := js.Global().Get("sessionStorage").Call("getItem", key)
	if value.IsNull() || value.IsUndefined() {
		return ""
	}
	return value.String()
}

// setSessionValue сохраняет значение в sessionStorage.
//
// Там лежит только проверочный код PKCE и он переживает ровно один
// редирект. Токен доступа туда не попадает: sessionStorage читает любой
// скрипт на странице.
func setSessionValue(key, value string) {
	js.Global().Get("sessionStorage").Call("setItem", key, value)
}

func clearSessionValue(key string) {
	js.Global().Get("sessionStorage").Call("removeItem", key)
}
