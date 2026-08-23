// Команда cover считает покрытие по объединённому профилю и проверяет порог.
//
// Штатного способа посчитать покрытие «без сгенерированного кода» в Go нет:
// go tool cover -func печатает всё подряд и не умеет ронять сборку. Поэтому
// профиль разбирается здесь: из него убирается то, что не тестируется
// по существу, остальное сводится по пакетам, а итог сравнивается с порогом.
//
// Список исключений ведётся руками в excluded и объясняет каждое из них.
// Дописывать туда пакет ради красивой цифры — значит врать самому себе:
// исключение уместно, только если код проверяется иначе или не может
// выполняться в тестовом бинарнике.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

var (
	profile = flag.String("profile", ".cover/all.out", "Файл профиля покрытия")
	root    = flag.String("root", ".", "Каталог репозитория")
	min     = flag.Float64("min", 0, "Минимально допустимое покрытие в процентах (0 — не проверять)")
	// module совпадает с путём модуля в go.mod: пути в профиле начинаются
	// с него, а на диске этому префиксу соответствует корень репозитория.
	module = flag.String("module", "wish", "Путь модуля")
)

// exclusion — одно исключение из подсчёта. Задаётся либо префиксом пути,
// либо именем функции; reason печатается в отчёте, чтобы список исключений
// был виден в том же выводе, что и цифра покрытия.
type exclusion struct {
	prefix   string
	function string
	reason   string
}

var excluded = []exclusion{
	{
		prefix: "wish/middleware/",
		reason: "сгенерировано protoc, проверяется make proto-check",
	},
	{
		prefix: "wish/services/cmd/web/app/",
		reason: "обвязка WebAssembly: syscall/js есть только в GOARCH=wasm",
	},
	{
		prefix: "wish/tools/",
		reason: "инструменты разработки; bookgen целиком прогоняется make docs-check",
	},
	{
		prefix: "wish/services/testsupport/",
		reason: "вспомогательный код тестов: ломается — падают сами тесты",
	},
	{
		function: "main",
		reason:   "точка входа: разбор флагов и сборка зависимостей",
	},
}

// block — один блок профиля: диапазон строк файла, число операторов в нём
// и признак того, выполнялся ли блок хотя бы раз.
type block struct {
	file      string
	startLine int
	endLine   int
	stmts     int
	covered   bool
}

func main() {
	flag.Parse()

	blocks, err := parseProfile(*profile)
	if err != nil {
		fail("разбор профиля: %v", err)
	}
	if len(blocks) == 0 {
		fail("профиль %s пуст", *profile)
	}

	kept, dropped, err := filter(blocks)
	if err != nil {
		fail("исключения: %v", err)
	}

	report(kept, dropped)

	total := percent(kept)
	if *min > 0 && total < *min {
		fmt.Fprintf(os.Stderr, "\nпокрытие %.1f%% ниже порога %.1f%%\n", total, *min)
		os.Exit(1)
	}
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}

// parseProfile читает профиль в формате go test -coverprofile.
// Строка выглядит как "путь/файл.go:9.13,11.2 1 0": диапазон, число
// операторов, счётчик выполнений.
func parseProfile(path string) ([]block, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() {
		// Файл открыт только на чтение: закрытие не может потерять данные.
		_ = f.Close()
	}()

	var blocks []block
	scanner := bufio.NewScanner(f)
	for line := 0; scanner.Scan(); line++ {
		text := strings.TrimSpace(scanner.Text())
		if text == "" || strings.HasPrefix(text, "mode:") {
			continue
		}
		b, err := parseBlock(text)
		if err != nil {
			return nil, fmt.Errorf("строка %d: %w", line+1, err)
		}
		blocks = append(blocks, b)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return blocks, nil
}

func parseBlock(text string) (block, error) {
	// Двоеточие ищется с конца: путь к файлу на Windows тоже может его содержать.
	colon := strings.LastIndex(text, ":")
	if colon < 0 {
		return block{}, fmt.Errorf("нет разделителя пути: %q", text)
	}
	file := text[:colon]

	fields := strings.Fields(text[colon+1:])
	if len(fields) != 3 {
		return block{}, fmt.Errorf("ожидалось три поля после пути: %q", text)
	}
	comma := strings.Index(fields[0], ",")
	if comma < 0 {
		return block{}, fmt.Errorf("нет разделителя диапазона: %q", text)
	}

	start, err := lineOf(fields[0][:comma])
	if err != nil {
		return block{}, err
	}
	end, err := lineOf(fields[0][comma+1:])
	if err != nil {
		return block{}, err
	}
	stmts, err := strconv.Atoi(fields[1])
	if err != nil {
		return block{}, fmt.Errorf("число операторов: %w", err)
	}
	count, err := strconv.Atoi(fields[2])
	if err != nil {
		return block{}, fmt.Errorf("счётчик: %w", err)
	}

	return block{file: file, startLine: start, endLine: end, stmts: stmts, covered: count > 0}, nil
}

// lineOf берёт номер строки из позиции вида "11.2".
func lineOf(pos string) (int, error) {
	dot := strings.Index(pos, ".")
	if dot < 0 {
		return 0, fmt.Errorf("позиция без колонки: %q", pos)
	}
	return strconv.Atoi(pos[:dot])
}

// filter делит блоки на учитываемые и исключённые. Второе значение
// возвращается не ради отладки: отчёт печатает, сколько операторов
// не попало в счёт, иначе исключения незаметно растут.
func filter(blocks []block) (kept, dropped []block, err error) {
	// Диапазоны исключаемых функций разбираются по одному файлу за раз:
	// парсить один и тот же файл на каждый блок незачем.
	ranges := make(map[string][][2]int)
	for _, b := range blocks {
		if _, ok := ranges[b.file]; ok {
			continue
		}
		if prefixExcluded(b.file) {
			ranges[b.file] = nil
			continue
		}
		r, err := functionRanges(b.file)
		if err != nil {
			return nil, nil, err
		}
		ranges[b.file] = r
	}

	for _, b := range blocks {
		if prefixExcluded(b.file) || inRanges(ranges[b.file], b) {
			dropped = append(dropped, b)
			continue
		}
		kept = append(kept, b)
	}
	return kept, dropped, nil
}

func prefixExcluded(file string) bool {
	for _, e := range excluded {
		if e.prefix != "" && strings.HasPrefix(file, e.prefix) {
			return true
		}
	}
	return false
}

// functionRanges возвращает диапазоны строк функций, исключённых по имени.
// Исключается именно функция, а не файл целиком: рядом с main в том же
// файле живёт обычный тестируемый код.
func functionRanges(file string) ([][2]int, error) {
	names := make(map[string]bool)
	for _, e := range excluded {
		if e.function != "" {
			names[e.function] = true
		}
	}
	if len(names) == 0 {
		return nil, nil
	}

	path, err := onDisk(file)
	if err != nil {
		return nil, err
	}
	fset := token.NewFileSet()
	parsed, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		return nil, fmt.Errorf("разбор %s: %w", path, err)
	}

	var ranges [][2]int
	for _, decl := range parsed.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv != nil || !names[fn.Name.Name] {
			continue
		}
		ranges = append(ranges, [2]int{
			fset.Position(fn.Pos()).Line,
			fset.Position(fn.End()).Line,
		})
	}
	return ranges, nil
}

// onDisk переводит путь из профиля (он начинается с пути модуля)
// в путь внутри репозитория.
func onDisk(file string) (string, error) {
	rel, ok := strings.CutPrefix(file, *module+"/")
	if !ok {
		return "", fmt.Errorf("путь %q вне модуля %q", file, *module)
	}
	return filepath.Join(*root, filepath.FromSlash(rel)), nil
}

func inRanges(ranges [][2]int, b block) bool {
	for _, r := range ranges {
		if b.startLine >= r[0] && b.endLine <= r[1] {
			return true
		}
	}
	return false
}

func percent(blocks []block) float64 {
	var total, covered int
	for _, b := range blocks {
		total += b.stmts
		if b.covered {
			covered += b.stmts
		}
	}
	if total == 0 {
		return 0
	}
	return float64(covered) / float64(total) * 100
}

func report(kept, dropped []block) {
	byPackage := make(map[string][]block)
	for _, b := range kept {
		byPackage[filepath.ToSlash(filepath.Dir(b.file))] = append(byPackage[filepath.ToSlash(filepath.Dir(b.file))], b)
	}

	packages := make([]string, 0, len(byPackage))
	for name := range byPackage {
		packages = append(packages, name)
	}
	sort.Strings(packages)

	width := 0
	for _, name := range packages {
		if len(name) > width {
			width = len(name)
		}
	}

	for _, name := range packages {
		blocks := byPackage[name]
		var stmts int
		for _, b := range blocks {
			stmts += b.stmts
		}
		fmt.Printf("%-*s  %6.1f%%  (%d операторов)\n", width, name, percent(blocks), stmts)
	}

	var droppedStmts int
	for _, b := range dropped {
		droppedStmts += b.stmts
	}
	fmt.Printf("\nисключено %d операторов:\n", droppedStmts)
	for _, e := range excluded {
		what := e.prefix
		if what == "" {
			what = "func " + e.function
		}
		fmt.Printf("  %-32s %s\n", what, e.reason)
	}

	fmt.Printf("\nвсего: %.1f%%\n", percent(kept))
}
