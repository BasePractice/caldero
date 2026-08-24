package services

import (
	"io/fs"
	"testing"
	"testing/fstest"
)

// TestLatestMigration проверяет разбор номеров версий. От него зависит,
// поднимется ли сервис: версия ниже ожидаемой означает, что схему
// не довели, и сервис обязан остановиться. Ошибиться здесь — значит либо
// останавливать рабочий сервис, либо пропускать отставшую схему.
func TestLatestMigration(t *testing.T) {
	tests := []struct {
		name    string
		files   []string
		want    uint64
		wantErr bool
	}{
		{
			name:  "пара up и down считается одной версией",
			files: []string{"1_init.up.sql", "1_init.down.sql"},
			want:  1,
		},
		{
			// Сортировка по имени ставит 10 перед 2, поэтому версия
			// ищется числом, а не строкой.
			name:  "двузначная версия старше однозначной",
			files: []string{"1_init.up.sql", "2_next.up.sql", "10_last.up.sql"},
			want:  10,
		},
		{
			name:  "посторонние файлы пропускаются",
			files: []string{"3_ok.up.sql", "README.md", "notes.txt"},
			want:  3,
		},
		{
			name:  "имя без разделителя пропускается",
			files: []string{"4_ok.up.sql", "broken.sql"},
			want:  4,
		},
		{
			name:    "нечисловой префикс — ошибка, а не тихий пропуск",
			files:   []string{"5_ok.up.sql", "next_step.up.sql"},
			wantErr: true,
		},
		{
			name:    "миграций нет вовсе",
			files:   []string{"README.md"},
			wantErr: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			files := fstest.MapFS{}
			for _, name := range test.files {
				files["migrations/"+name] = &fstest.MapFile{Data: []byte("SELECT 1;")}
			}
			// Каталог внутри: он не миграция, и считать его версией нельзя.
			files["migrations/archive"] = &fstest.MapFile{Mode: fs.ModeDir | 0o755}

			got, err := latestMigration(files)
			if test.wantErr {
				if err == nil {
					t.Fatalf("ожидалась ошибка, получена версия %d", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("разбор версий: %v", err)
			}
			if got != test.want {
				t.Errorf("версия %d, ожидалась %d", got, test.want)
			}
		})
	}
}

// TestLatestMigrationWithoutDirectory: каталога миграций может не быть
// вовсе — это ошибка сборки образа, и молчать о ней нельзя.
func TestLatestMigrationWithoutDirectory(t *testing.T) {
	if _, err := latestMigration(fstest.MapFS{}); err == nil {
		t.Fatal("отсутствие каталога миграций принято за успех")
	}
}
