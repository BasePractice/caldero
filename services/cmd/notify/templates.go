package main

import (
	"embed"
	"fmt"
	"strings"
	"text/template"

	"wish/services/shared/notify"
)

//go:embed templates/*.tmpl
var templateFiles embed.FS

// Templates собирает тексты оповещений.
//
// text/template, а не html/template: сообщение уходит в мессенджер
// и в ленту приложения обычным текстом, а экранирование под HTML
// исказило бы кавычки и амперсанды в названиях товаров.
type Templates struct {
	tmpl *template.Template
}

// LoadTemplates читает шаблоны и проверяет, что для каждого известного
// события есть заголовок и тело. Проверка при старте, а не при отправке:
// пропущенный шаблон иначе обнаружится в тот момент, когда сообщение
// уже нужно доставить.
func LoadTemplates() (*Templates, error) {
	// missingkey=error: сообщение с дырой посреди фразы хуже, чем
	// недоставленное, — такую ошибку видно в очереди, а кривой текст
	// увидит только пользователь.
	tmpl, err := template.New("messages").Option("missingkey=error").
		ParseFS(templateFiles, "templates/*.tmpl")
	if err != nil {
		return nil, fmt.Errorf("parsing message templates: %w", err)
	}

	var missing []string
	for _, eventType := range notify.EventTypes() {
		for _, part := range []string{"title", "body"} {
			name := string(eventType) + "." + part
			if tmpl.Lookup(name) == nil {
				missing = append(missing, name)
			}
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("missing message templates: %s", strings.Join(missing, ", "))
	}
	return &Templates{tmpl: tmpl}, nil
}

// Render собирает заголовок и тело сообщения.
func (t *Templates) Render(
	eventType notify.EventType,
	payload map[string]string,
) (title string, body string, err error) {
	if title, err = t.render(string(eventType)+".title", payload); err != nil {
		return "", "", err
	}
	if body, err = t.render(string(eventType)+".body", payload); err != nil {
		return "", "", err
	}
	return title, body, nil
}

func (t *Templates) render(name string, payload map[string]string) (string, error) {
	var out strings.Builder
	if err := t.tmpl.ExecuteTemplate(&out, name, payload); err != nil {
		return "", fmt.Errorf("rendering %s: %w", name, err)
	}
	return strings.TrimSpace(out.String()), nil
}
