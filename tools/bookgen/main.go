// Команда bookgen собирает из исходников проекта те части документа,
// которые нельзя писать руками.
//
// Смысл разделения простой: текст, написанный отдельно от кода, устаревает
// через месяц. Поэтому руками пишется только то, что объясняет решения,
// а списки эндпоинтов, переменных, требований и сервисов выводятся
// из самих исходников. Расхождение ловит CI: он перегенерирует файлы
// и сравнит с зафиксированными, как это делает go mod tidy -diff.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

var (
	root = flag.String("root", ".", "Каталог репозитория")
	// out позволяет собрать файлы во временный каталог: проверка
	// расхождения не должна менять рабочее дерево.
	out = flag.String("out", "", "Каталог для собранных файлов (по умолчанию docs/book/generated)")
)

func main() {
	flag.Parse()

	target := *out
	if target == "" {
		target = filepath.Join(*root, "docs", "book", "generated")
	}
	if err := os.MkdirAll(target, 0o755); err != nil {
		fail("создание каталога: %v", err)
	}

	tasks := []struct {
		name  string
		build func(string) (string, error)
	}{
		{"requirements.typ", requirements},
		{"endpoints.typ", endpoints},
		{"env.typ", environment},
		{"adr.typ", decisions},
		{"services.typ", servicesChapter},
	}
	for _, task := range tasks {
		content, err := task.build(*root)
		if err != nil {
			fail("%s: %v", task.name, err)
		}
		if err = os.WriteFile(filepath.Join(target, task.name), []byte(content), 0o644); err != nil {
			fail("запись %s: %v", task.name, err)
		}
		fmt.Println("собрано", task.name)
	}
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}

// header — общее начало сгенерированного файла. Оно предупреждает того,
// кто попытается править файл руками.
func header(source string) string {
	return fmt.Sprintf("// Этот файл собран командой bookgen из %s.\n"+
		"// Править его руками бессмысленно: он перезаписывается,\n"+
		"// а расхождение с исходником роняет сборку.\n\n", source)
}

// escape готовит текст для Typst: разметочные знаки экранируются,
// иначе имя вроде user_id превращается в подстрочный индекс.
func escape(text string) string {
	replacer := strings.NewReplacer(
		"\\", "\\\\", "#", "\\#", "$", "\\$", "*", "\\*", "_", "\\_",
		"`", "\\`", "<", "\\<", ">", "\\>", "@", "\\@", "[", "\\[", "]", "\\]",
	)
	return replacer.Replace(text)
}

// status переводит значки реестра требований в слова: значок зависит
// от того, какие глифы есть в шрифте, а слово — нет.
func status(mark string) string {
	switch strings.TrimSpace(mark) {
	case "✅":
		return "готово"
	case "🟡":
		return "частично"
	case "⬜":
		return "нет"
	default:
		return strings.TrimSpace(mark)
	}
}

// markdownRows разбирает строки markdown-таблицы из указанного раздела.
func markdownRows(text, section string) [][]string {
	lines := strings.Split(text, "\n")
	rows := make([][]string, 0)

	inSection := section == ""
	for _, line := range lines {
		if strings.HasPrefix(line, "#") {
			inSection = section != "" && strings.Contains(line, section)
			continue
		}
		if !inSection || !strings.HasPrefix(line, "|") {
			continue
		}
		// Разделитель шапки и сама шапка в данные не попадают.
		if strings.Contains(line, "---") {
			continue
		}

		cells := strings.Split(strings.Trim(line, "|"), "|")
		for i := range cells {
			cells[i] = strings.TrimSpace(cells[i])
		}
		if len(cells) > 0 && (cells[0] == "№" || cells[0] == "Метод" || cells[0] == "Сервис") {
			continue
		}
		rows = append(rows, cells)
	}
	return rows
}

// requirements собирает реестр требований.
func requirements(root string) (string, error) {
	source := filepath.Join(root, "docs", "requirements.md")
	raw, err := os.ReadFile(source)
	if err != nil {
		return "", err
	}
	text := string(raw)

	out := &strings.Builder{}
	out.WriteString(header("docs/requirements.md"))
	out.WriteString("= Требования\n\n")
	out.WriteString("Реестр ведётся в `docs/requirements.md`; здесь он приведён целиком.\n\n")

	sections := []struct{ title, marker string }{
		{"Функциональные требования", ""},
		{"Нефункциональные требования", "Нефункциональные"},
		{"Внешние ограничения", "Внешние ограничения"},
	}
	for _, section := range sections {
		rows := markdownRows(text, section.marker)
		if section.marker == "" {
			// Функциональные требования — всё, что до нефункциональных.
			cut := strings.Index(text, "## Нефункциональные")
			rows = markdownRows(text[:cut], "")
		}
		if len(rows) == 0 {
			continue
		}

		fmt.Fprintf(out, "== %s\n\n", section.title)
		columns := 5
		if section.marker == "Внешние ограничения" {
			columns = 2
		}
		fmt.Fprintf(out, "#table(\n  columns: (auto, 1fr, auto, auto),\n"+
			"  align: (left, left, left, left),\n  table.header([*№*], [*Требование*], [*Критерий*], [*Статус*]),\n")
		for _, row := range rows {
			switch {
			case columns == 2 && len(row) >= 2:
				fmt.Fprintf(out, "  [%s], [%s], [], [],\n", escape(row[0]), escape(row[1]))
			case len(row) >= 5:
				fmt.Fprintf(out, "  [%s], [%s], [%s], [%s],\n",
					escape(row[0]), escape(row[1]), escape(row[3]), status(row[4]))
			}
		}
		out.WriteString(")\n\n")
	}
	return out.String(), nil
}

// endpointEntry — эндпоинт шлюза.
type endpointEntry struct {
	Comment  string `json:"@comment"`
	Endpoint string `json:"endpoint"`
	Method   string `json:"method"`
	Backend  []struct {
		Host       []string `json:"host"`
		URLPattern string   `json:"url_pattern"`
	} `json:"backend"`
	Extra map[string]json.RawMessage `json:"extra_config"`
}

var envPlaceholder = regexp.MustCompile(`\{\{ env "[A-Z_]+" \}\}`)

// endpoints собирает внешний API из конфигурации шлюза.
//
// Источник именно шлюз, а не обработчики сервисов: снаружи доступно
// ровно то, что он проксирует, и документ должен описывать эту границу,
// а не внутренние маршруты.
func endpoints(root string) (string, error) {
	source := filepath.Join(root, "config", "krakend", "krakend.tmpl")
	raw, err := os.ReadFile(source)
	if err != nil {
		return "", err
	}

	var config struct {
		Endpoints []endpointEntry `json:"endpoints"`
	}
	if err = json.Unmarshal(envPlaceholder.ReplaceAll(raw, []byte("PLACEHOLDER")), &config); err != nil {
		return "", fmt.Errorf("разбор шаблона шлюза: %w", err)
	}

	out := &strings.Builder{}
	out.WriteString(header("config/krakend/krakend.tmpl"))
	out.WriteString("= Внешний API\n\n")
	out.WriteString("Снаружи доступно ровно то, что проксирует шлюз. " +
		"Внутренние порты сервисов не публикуются.\n\n")
	out.WriteString("#table(\n  columns: (auto, 1fr, auto, auto),\n" +
		"  align: (left, left, left, left),\n" +
		"  table.header([*Метод*], [*Путь*], [*Сервис*], [*Доступ*]),\n")

	for _, endpoint := range config.Endpoints {
		service := "—"
		if len(endpoint.Backend) > 0 && len(endpoint.Backend[0].Host) > 0 {
			host := endpoint.Backend[0].Host[0]
			host = strings.TrimPrefix(host, "http://")
			if index := strings.Index(host, ":"); index > 0 {
				host = host[:index]
			}
			service = host
		}
		access := "публичный"
		if _, ok := endpoint.Extra["auth/validator"]; ok {
			access = "по токену"
		}
		fmt.Fprintf(out, "  [%s], [%s], [%s], [%s],\n",
			escape(endpoint.Method), escape(endpoint.Endpoint), escape(service), access)
	}
	out.WriteString(")\n\n")
	return out.String(), nil
}

// environment собирает переменные окружения вместе с их объяснениями.
func environment(root string) (string, error) {
	source := filepath.Join(root, ".env.example")
	raw, err := os.ReadFile(source)
	if err != nil {
		return "", err
	}

	out := &strings.Builder{}
	out.WriteString(header(".env.example"))
	out.WriteString("= Переменные окружения\n\n")
	out.WriteString("Комментарии взяты из `.env.example`: там они объясняют, " +
		"почему переменная существует и чем грозит её пустое значение.\n\n")

	comment := make([]string, 0, 4)
	for _, line := range strings.Split(string(raw), "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, "#"):
			comment = append(comment, strings.TrimSpace(strings.TrimPrefix(trimmed, "#")))
		case trimmed == "":
			comment = comment[:0]
		default:
			name, value, found := strings.Cut(trimmed, "=")
			if !found {
				continue
			}
			fmt.Fprintf(out, "/ %s: %s", escape(name), escape(strings.Join(comment, " ")))
			if value != "" {
				fmt.Fprintf(out, " По умолчанию: `%s`.", value)
			}
			out.WriteString("\n\n")
			comment = comment[:0]
		}
	}
	return out.String(), nil
}

// decisions собирает список принятых решений.
func decisions(root string) (string, error) {
	source := filepath.Join(root, "docs", "adr", "README.md")
	raw, err := os.ReadFile(source)
	if err != nil {
		return "", err
	}

	out := &strings.Builder{}
	out.WriteString(header("docs/adr/"))
	out.WriteString("= Принятые решения\n\n")
	out.WriteString("Каждое решение записано с контекстом, альтернативами и условиями " +
		"пересмотра. Полные тексты — в `docs/adr/`.\n\n")

	link := regexp.MustCompile(`\[(\d+)\]\(([^)]+)\)`)
	for _, row := range markdownRows(string(raw), "") {
		if len(row) < 3 {
			continue
		}
		match := link.FindStringSubmatch(row[0])
		if match == nil {
			continue
		}
		fmt.Fprintf(out, "/ ADR %s: %s (%s)\n\n", match[1], escape(row[1]), escape(row[2]))
	}
	return out.String(), nil
}

// servicesChapter собирает состав системы из самих сервисов.
func servicesChapter(root string) (string, error) {
	pattern := filepath.Join(root, "services", "cmd", "*")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return "", err
	}
	sort.Strings(matches)

	port := regexp.MustCompile(`flag\.Int\("port", (\d+)`)

	out := &strings.Builder{}
	out.WriteString(header("services/cmd/"))
	out.WriteString("= Сервисы\n\n")
	out.WriteString("Состав системы собран из самих сервисов: порт берётся из точки входа, " +
		"наличие схемы — из каталога миграций.\n\n")
	out.WriteString("#table(\n  columns: (auto, auto, auto, 1fr),\n" +
		"  align: (left, left, left, left),\n" +
		"  table.header([*Сервис*], [*Порт*], [*Схема*], [*Назначение*]),\n")

	for _, match := range matches {
		info, err := os.Stat(match)
		if err != nil || !info.IsDir() {
			continue
		}
		name := filepath.Base(match)

		main, err := os.ReadFile(filepath.Join(match, "main.go"))
		if err != nil {
			continue
		}
		portValue := "—"
		if found := port.FindSubmatch(main); found != nil {
			portValue = string(found[1])
		}

		schema := "—"
		if _, err = os.Stat(filepath.Join(match, "migrations")); err == nil {
			schema = name
		}

		fmt.Fprintf(out, "  [%s], [%s], [%s], [%s],\n",
			escape(name), portValue, escape(schema), escape(purpose(match)))
	}
	out.WriteString(")\n\n")
	return out.String(), nil
}

// purpose достаёт назначение сервиса из первого абзаца README.
//
// Абзац, а не строка: описание обычно занимает две-три строки, и обрывать
// его на первой значит показывать половину предложения. Заголовки, списки
// и таблицы пропускаются — там уже подробности, а не назначение.
func purpose(dir string) string {
	raw, err := os.ReadFile(filepath.Join(dir, "README.md"))
	if err != nil {
		return "описание не заведено"
	}

	paragraph := make([]string, 0, 4)
	for _, line := range strings.Split(string(raw), "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case trimmed == "":
			if len(paragraph) > 0 {
				return strings.TrimSuffix(strings.Join(paragraph, " "), ".")
			}
		case strings.HasPrefix(trimmed, "#"), strings.HasPrefix(trimmed, "|"),
			strings.HasPrefix(trimmed, "-"), strings.HasPrefix(trimmed, "*"),
			isNumbered(trimmed):
			paragraph = paragraph[:0]
		default:
			paragraph = append(paragraph, trimmed)
		}
	}
	if len(paragraph) > 0 {
		return strings.TrimSuffix(strings.Join(paragraph, " "), ".")
	}
	return "описание не заведено"
}

// isNumbered отличает пункт нумерованного списка от обычной строки.
func isNumbered(line string) bool {
	digits := 0
	for digits < len(line) && line[digits] >= '0' && line[digits] <= '9' {
		digits++
	}
	return digits > 0 && digits < len(line) && line[digits] == '.'
}
