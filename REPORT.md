# Отчёт по репозиторию `caldero` (модуль `wish`)

**Дата анализа:** 2026-08-23
**Ветка:** `stage/1` (HEAD `d70ad35`), рабочее дерево чистое
**Go:** объявлен `go 1.24.2`, проверено на `go1.27.0 darwin/arm64`

---

## 1. Резюме

Учебный pet-проект «Котелок» — заготовка микросервисной платформы на Go: четыре сервиса
(`users`, `credit`, `account`, `wallet`), общий пакет `services`, gRPC-контракт для кошелька,
и обширный docker-compose со всей обвязкой наблюдаемости (KrakenD, Keycloak, Prometheus,
Grafana, ELK, Jaeger, InfluxDB, Redis, PostgreSQL).

Состояние по факту:

| Область | Оценка |
|---|---|
| Компиляция (`go build ./...`) | ✅ проходит |
| `go vet ./...` | ✅ чисто |
| `gofmt -l .` | ✅ чисто |
| Тесты (`go test ./...`) | ⚠️ проходят, но покрыт **один** файл из 26 (`payments.go`) |
| Работоспособность в рантайме | ❌ большинство ключевых сценариев падают или не реализованы |
| Безопасность | ❌ есть критические дыры (полный обход аутентификации, IDOR) |
| Соответствие README | ❌ реализовано ~10 % заявленной функциональности |

**Главный вывод:** проект собирается, но не работает. «Зелёная» сборка обманчива —
провалы спрятаны в SQL-строках, в заглушках `panic("implement me")` и в порядке
инициализации. Ниже — детальный разбор.

---

## 2. Что и как проверялось

```bash
go build ./...      # exit 0, ошибок нет
go vet ./...        # пусто
gofmt -l .          # пусто
go test ./...       # ok wish/services/cmd/credit; остальные — no test files
```

Статический анализ (`staticcheck`, `golangci-lint`) в системе не установлен и в репозитории
не сконфигурирован, CI (`.github/`) отсутствует — то есть ничего из перечисленного
не выполняется автоматически ни при коммите, ни при сборке.

---

## 3. Ошибки сборки и сборочная инфраструктура

Ошибок компиляции нет. Проблемы — в организации сборки.

### 3.1. `go mod tidy` внутри Dockerfile
[services/cmd/account/Dockerfile:8](services/cmd/account/Dockerfile:8) (и три идентичных файла)

```dockerfile
RUN go mod download
RUN go mod tidy
```

`go mod tidy` в образе выполняется **до** `COPY . .`, то есть при отсутствии исходников
он вычистит `go.mod`/`go.sum` до пустого состояния и следующий `go build` полезет в сеть
за зависимостями заново. Сборка недетерминирована и зависит от доступности прокси-модулей.
Строку нужно убрать: `go mod download` достаточно, а расхождение `go.mod` должно ловиться
в CI командой `go mod tidy -diff`.

### 3.2. Четыре почти идентичных Dockerfile
Отличаются только именем бинарника и портом. Каждый выполняет полный `COPY . .`, из-за чего
изменение в любом сервисе инвалидирует слои всех четырёх образов. Решение — один
параметризованный Dockerfile с `ARG SERVICE`, либо `--target` в multi-stage.

### 3.3. Образ `FROM scratch` без сопутствующих файлов
- Нет `ca-certificates` — любой будущий исходящий HTTPS-вызов (маркетплейсы, платёжки,
  JWKS Keycloak) упадёт с `x509: certificate signed by unknown authority`.
- Нет `tzdata` — `time.Now()` и `AddDate` работают в UTC, что для графика платежей
  по кредиту имеет значение.
- Процесс запускается от `uid 0`. Нужен `USER 65534:65534`.
- Нет `HEALTHCHECK`, из-за чего `depends_on: condition: service_healthy` для этих
  сервисов в принципе невозможен.

### 3.4. Устаревший синтаксис
`FROM golang:1.24-alpine3.21 as builder` — `as` в нижнем регистре, BuildKit выдаёт
предупреждение `FromAsCasing`. Нужно `AS`.

### 3.5. `build.sh` / `build.cmd`
- Хардкод путей: `E:\Programs\protobuf\bin` в [build.cmd:3](build.cmd:3), `~/go/bin`
  в [build.sh:3](build.sh:3).
- Нет `set -e` в [build.sh](build.sh) — при падении `protoc` скрипт продолжит собирать
  бинарники со старым сгенерированным кодом и завершится с кодом 0.
- Отсутствует `.gitignore`-запись на артефакты `protoc`, при этом сгенерированные
  `*.pb.go` закоммичены — то есть источник истины двоится: генерация может разойтись
  с содержимым репозитория и это никем не проверяется.
- Нет цели `test`, `vet`, `lint`. Makefile отсутствует, хотя [config/keycloak/README.md](config/keycloak/README.md)
  ссылается на несуществующую команду `make save-keycloak-config`.

---

## 4. Критические дефекты в коде

### 4.1. 🔴 `.env` читается после инициализации конфигурации — переменные не применяются
[services/cmd/users/main.go:34-40](services/cmd/users/main.go:34) (и аналогично во всех
четырёх `main.go`)

```go
services.DefineLogging()          // уже использует services.LogLevel
services.DefineMetrics()
err := godotenv.Load(".env", ".env.local")   // ← слишком поздно
```

Переменные `PostgresUrl`, `RedisUrl`, `LogLevel`, `MetricsPort` — это **пакетные
переменные** в [services/properties.go:5-15](services/properties.go:5), они вычисляются
при инициализации пакета, то есть до входа в `main`. Всё, что `godotenv` положит
в окружение, уже никем не читается. Файл `.env` в репозитории отсутствует, поэтому
на каждом старте гарантированно печатается `Warning loading .env file`, и все
настройки берутся из дефолтов.

Дефолт при этом заведомо нерабочий:
```go
"postgres://postgres:postgres@local:25432/wish?..."   // хост "local" не резолвится
```

**Исправление:** загружать `.env` первым делом в `init()` отдельного пакета конфигурации
либо перейти на явную функцию `LoadConfig()`, вызываемую до всего остального.

### 4.2. 🔴 Ключи подписи JWT не читаются — запрос по несуществующей колонке
[services/cmd/users/db.go:100](services/cmd/users/db.go:100)

```go
"SELECT private_key FROM keys WHERE id = $1"
```

В схеме [services/cmd/users/migrations/1_init_schema.up.sql](services/cmd/users/migrations/1_init_schema.up.sql)
таблица `keys` имеет колонки `key_id`, `private_key`, `created_at`. Колонки `id` **нет**.
Запись ключа идёт в `key_id` ([db.go:109](services/cmd/users/db.go:109)), чтение — по `id`.

Последствие: `GetPrivateKey` и `GetPublicKey` всегда возвращают
`pq: column "id" does not exist`. Подпись токенов невозможна, `/token` и `/auth`
не работают. Должно быть `WHERE key_id = $1`.

### 4.3. 🔴 `ClearKeys` использует SQLite-синтаксис в PostgreSQL
[services/cmd/users/db.go:117-124](services/cmd/users/db.go:117)

```sql
DELETE FROM keys WHERE rowid NOT IN (SELECT rowid FROM keys ORDER BY created_at DESC LIMIT 2)
```

В PostgreSQL нет псевдоколонки `rowid` (аналог — `ctid`, но его нельзя использовать так
наивно). Запрос всегда падает. `RotateKeys` возвращает эту ошибку наружу
([keys.go:86](services/cmd/users/keys.go:86)), из-за чего инициализация `KeyManager`
на первом старте завершается ошибкой.

Корректный вариант:
```sql
DELETE FROM keys WHERE key_id NOT IN (SELECT key_id FROM keys ORDER BY created_at DESC LIMIT 2)
```

### 4.4. 🔴 Ошибка `NewKeyManager` игнорируется
[services/cmd/users/service.go:40](services/cmd/users/service.go:40)

```go
keyManager, _ := NewKeyManager(ctx, db)
```

С учётом п. 4.3 здесь всегда `nil`. Дальше `keyManager.GetPrivateKey` передаётся
в `compose.Compose` как метод nil-интерфейса → паника при первом обращении к `/token`.
Ошибку нужно обрабатывать и падать на старте (fail fast), а не запускать заведомо
нерабочий сервис.

### 4.5. 🔴 Смешение стилей плейсхолдеров + перепутанные аргументы
[services/cmd/users/db.go:257-262](services/cmd/users/db.go:257)

```go
s.db.Exec("DELETE FROM oauth_tokens WHERE signature = ? AND token_type = $1", signature, tokenType)
```

`lib/pq` понимает только `$N`. Запрос содержит один плейсхолдер `$1`, а аргументов
передано два, при этом `$1` по смыслу должен быть `signature`, а получит `tokenType`.
Отзыв токена не работает никогда. Должно быть:
```go
"DELETE FROM oauth_tokens WHERE signature = $1 AND token_type = $2"
```

### 4.6. 🔴 Authorization Code Flow не реализован, но объявлен
[services/cmd/users/db.go:54-72](services/cmd/users/db.go:54)

```go
func (s *ds) CreateAuthorizeCodeSession(...) { panic("implement me") }
func (s *ds) GetAuthorizeCodeSession(...)    { panic("implement me") }
func (s *ds) InvalidateAuthorizeCodeSession(...) { panic("implement me") }
func (s *ds) RotateRefreshToken(...)         { panic("implement me") }
```

При этом в [service.go:49](services/cmd/users/service.go:49) подключён
`compose.OAuth2AuthorizeExplicitFactory`, а клиент `test-client` в миграции создан
с `grant_types = 'authorization_code,refresh_token,password'`. Любой поход
на `/auth` → паника в обработчике. `net/http` перехватит её и оборвёт соединение,
но `defer recover()` в `main` тут не помогает — он в другой горутине и вводит в
заблуждение относительно устойчивости сервиса.

Либо реализовать хранилище кодов, либо убрать фабрику и оставить только
`password` + `refresh_token` grant.

### 4.7. 🔴 `/me` не может работать в принципе
[services/cmd/users/service.go:170-194](services/cmd/users/service.go:170)

`CoreStrategy` сконфигурирован как `compose.NewOAuth2HMACStrategy` — access-токен
представляет собой непрозрачную строку вида `<signature>.<key>`, а не JWT.
В `protect` этот же токен затем подаётся в `jwt.ParseWithClaims`, который ожидает
три base64-сегмента. Разбор всегда завершится ошибкой → 401 на любой валидный токен.

Нужно либо перейти на `compose.NewOAuth2JWTStrategy`, либо брать claims
из результата `IntrospectToken` (он уже возвращает `AccessRequester` с сессией),
который сейчас отбрасывается через `_, _, err :=`.

### 4.8. 🔴 Секрет OAuth2-клиента хранится в открытом виде
[services/cmd/users/migrations/1_init_schema.up.sql](services/cmd/users/migrations/1_init_schema.up.sql)

```sql
INSERT INTO oauth_clients(...) VALUES ('test-client', 'test-secret', ...)
```

`fosite.DefaultClient.GetHashedSecret()` возвращает поле `Secret`, которое сравнивается
через bcrypt-хешер. Незахешированное значение сравнение не пройдёт **никогда** —
аутентификация клиента сломана даже после исправления пп. 4.2–4.6. Секрет
необходимо хранить как bcrypt-хеш (и не в миграции).

### 4.9. 🔴 IDOR: чтение чужого кошелька
[services/cmd/wallet/service.go:24-32](services/cmd/wallet/service.go:24)

```go
if request.UserId == nil {
    userId = authorized.Id
} else {
    userId, err = uuid.Parse(*request.UserId)   // ← произвольный чужой UUID
}
```

Любой аутентифицированный пользователь, передав чужой `user_id`, получает баланс,
количество транзакций и суммы резервов чужого кошелька. Проверки прав нет вообще.
Требуется либо сверка `request.UserId == authorized.Id`, либо явная роль
администратора/сервисного аккаунта.

### 4.10. 🔴 Обработчики продолжают выполнение после ошибки валидации
[services/cmd/credit/handlers.go:69-88](services/cmd/credit/handlers.go:69),
[services/cmd/account/handlers.go:44-63](services/cmd/account/handlers.go:44)

```go
err := json.NewDecoder(r.Body).Decode(&c)
if err != nil {
    w.WriteHeader(http.StatusBadRequest)      // ← нет return
} else if !c.Validate() {
    w.WriteHeader(http.StatusBadRequest)      // ← нет return
}
id, err := db.Create(ctx, c, operator)        // выполняется в любом случае
```

Невалидный кредит всё равно уходит в БД. Клиент при этом уже получил 400, а следующий
`w.WriteHeader` печатает в лог `superfluous response.WriteHeader call`. Для `account`
это ещё и гарантированная паника (`db.Create` — заглушка).

### 4.11. 🔴 Возможное переполнение и OOM в расчёте графика платежей
[services/cmd/credit/payments.go:33-37](services/cmd/credit/payments.go:33)

```go
var needMonth = credit.Month - alreadyPaidMonth      // uint: при alreadyPaidMonth > Month → ~1.8e19
...
payments := make([]MonthPayment, needMonth)          // паника/OOM
```

`Month` и `alreadyPaidMonth` — беззнаковые. Если `last_payed_at` окажется дальше
`created_at + Month`, вычитание уйдёт в переполнение и `make` попытается выделить
десятки эксабайт. Плюс два вырожденных случая рядом:
- `needMonth == 0` → `math.Pow(1+p, 0) - 1 == 0` → деление на ноль → `+Inf` →
  `uint(+Inf)` — неопределённое поведение;
- `Percent == 0` → `monthPercent == 0` → `0/0` → `NaN`.

Нужны явные проверки границ до вычислений.

### 4.12. 🔴 Расчёт кредита ведётся по неполным данным
[services/cmd/credit/db.go:30-32](services/cmd/credit/db.go:30)

```sql
SELECT user_id, creator_id, type, percent, balance, kind, month FROM credit WHERE id = $1
```

Не выбираются `already_payed`, `created_at`, `last_payed_at` — а именно они управляют
логикой в `mothPaymentCalculation`. В результате из БД всегда приходит
`AlreadyPayed = 0`, `LastPayedAt = nil`, и график для частично погашенного кредита
считается как для нового. Ветка «кредит с оплатой», ради которой написан второй
подтест, в продакшн-пути недостижима.

### 4.13. 🟠 Некорректный SQL-агрегат в кошельке
[services/cmd/wallet/db.go:33-44](services/cmd/wallet/db.go:33)

```sql
trans_debit AS (SELECT SUM(t.value) AS value, wlt.id
                FROM transaction t LEFT JOIN wlt ON t.target = wlt.id
                WHERE ... GROUP BY t.value, wlt.id)
```

`GROUP BY t.value` разбивает агрегат по каждому уникальному номиналу: вместо одной
строки с суммой резервов получается по строке на каждую сумму. При последующем
`LEFT JOIN` это размножает строки кошелька — сервис вернёт один и тот же кошелёк
N раз с частичными суммами. Должно быть `GROUP BY wlt.id`.

Дополнительно: `LEFT JOIN wlt` вместо `JOIN` заставляет сканировать всю таблицу
`transaction` целиком (включая транзакции всех прочих пользователей) прежде,
чем отбросить их join'ом — на растущей таблице это станет главным тормозом.

### 4.14. 🟠 `goto` с гонкой при автосоздании кошелька
[services/cmd/wallet/db.go:27-89](services/cmd/wallet/db.go:27)

Схема «нет строк → `INSERT` → `goto next`» при параллельных запросах одного
пользователя даст нарушение `UNIQUE (user_id, type)` и вернёт ошибку вместо кошелька.
Нужен `INSERT ... ON CONFLICT DO NOTHING` (и лучше — один запрос вместо цикла).
Также `defer rows.Close()` внутри цикла с `goto` накапливает отложенные вызовы
на каждой итерации.

### 4.15. 🟠 Форматирование UUID через `%d`
[services/cmd/account/handlers.go:61](services/cmd/account/handlers.go:61)

```go
w.Header().Set("X-Account-Id", fmt.Sprintf("%d", id))   // id — *uuid.UUID
```

Вернёт `&[16 200 ...]` — массив байт, а не идентификатор. Нужен `id.String()`.
`go vet` это не ловит, потому что `%d` формально применим к массиву байт.

### 4.16. 🟠 Мёртвый код: создание OAuth2-клиента недоступно
[services/cmd/users/service.go:63](services/cmd/users/service.go:63)

`handleCreateClient` реализован, но не зарегистрирован в
[handlers.go:8-17](services/cmd/users/handlers.go:8). Единственный способ завести
клиента — `INSERT` в миграции. Функция также игнорирует ошибку
(`CreateClient` не возвращает `error`, [db.go:74](services/cmd/users/db.go:74))
и не отдаёт клиенту никакого статуса.

### 4.17. 🟠 `ExitHandle` не обеспечивает graceful shutdown
[services/exit.go:11-19](services/exit.go:11)

```go
func ExitHandle(handler ExitHandler) context.Context {
    parent := context.Background()
    go func() { ... handler(ctx) }()
    return parent            // ← возвращается контекст, который никогда не отменяется
}
```

Возвращаемый контекст — это `context.Background()`, он не связан с сигналом.
Именно он раздаётся в обработчики и в БД-запросы, то есть отмена по SIGTERM
до них не доходит. Сам обработчик делает `os.Exit(0)` немедленно — активные
HTTP-запросы и gRPC-стримы обрываются, соединения с БД не закрываются.
Нужен `signal.NotifyContext` с возвратом производного контекста и
`srv.Shutdown(ctx)` / `grpcServer.GracefulStop()`.

### 4.18. 🟠 Флаг регистрируется после `flag.Parse()`
[services/logging.go:50](services/logging.go:50) вызывается из `main` **после**
`flag.Parse()` ([main.go:33-35](services/cmd/users/main.go:33)). Флаг `-mport`
регистрируется, но никогда не парсится: `*mPort` всегда равен значению из
переменной окружения. Хуже — попытка реально передать `-mport` завершится
`flag provided but not defined`, потому что на момент парсинга флага ещё нет.

### 4.19. 🟠 Ошибки миграций только логируются
[services/db.go:14-34](services/db.go:14)

```go
d, err := iofs.New(migrations, "migrations")
if err != nil {
    slog.Error(...)      // ← нет return, дальше используется невалидный d
}
```

Первая ветка не делает `return`. Более того, `NewDatabase` возвращает
`(*sql.DB, nil)` даже если ни одна миграция не применилась — сервис поднимется
с отсутствующей или устаревшей схемой и будет сыпать ошибками в рантайме.
`migrationScheme` должна возвращать `error`, а вызывающая сторона — падать.

### 4.20. 🟡 `sql.Open` без проверки соединения и без настройки пула
[services/db.go:36-43](services/db.go:36) — нет `db.PingContext`,
нет `SetMaxOpenConns` / `SetMaxIdleConns` / `SetConnMaxLifetime`. При дефолтах
`database/sql` пул неограничен, что под нагрузкой упрётся в `max_connections`
PostgreSQL (по умолчанию 100 на все четыре сервиса вместе).

### 4.21. 🟡 Запросы без контекста
[services/cmd/wallet/db.go:29](services/cmd/wallet/db.go:29) — `d.db.Query`,
[services/cmd/users/db.go:163,219,234,257,266,274](services/cmd/users/db.go:163) —
`s.db.QueryRow` / `s.db.Exec`. Отмена запроса при разрыве клиентского соединения
не работает, таймаут не задаётся. Везде должны быть `...Context`-варианты
с реальным контекстом запроса.

### 4.22. 🟡 Строковый ключ в `context.WithValue`
[services/cmd/users/service.go:192](services/cmd/users/service.go:192)

```go
context.WithValue(r.Context(), "claims", claims)
```

Нетипизированный строковый ключ — риск коллизии между пакетами (`staticcheck SA1029`).
Нужен приватный тип `type ctxKey struct{}`. Соответственно
[protected.go:12](services/cmd/users/protected.go:12) делает
`ctx.Value("claims").(jwt.MapClaims)` без проверки `ok` — паника, если значения нет.

### 4.23. 🟡 `w.WriteHeader` после записи тела
[services/cmd/users/protected.go:18-23](services/cmd/users/protected.go:18) —
сначала `w.Write(bytes)`, потом `w.WriteHeader(http.StatusOK)`. Второй вызов
не имеет эффекта и логируется как `superfluous`.

### 4.24. 🟡 `errors.New(fmt.Sprintf(...))`
[services/authorization.go:21,39](services/authorization.go:21) — идиоматично
`fmt.Errorf("invalid user id: %w", err)`, к тому же сохраняется цепочка ошибок.

### 4.25. 🟡 Конструкторы возвращают `nil` вместо ошибки
[services/cmd/users/db.go:301](services/cmd/users/db.go:301),
[services/cmd/credit/db.go:50](services/cmd/credit/db.go:50),
[services/cmd/account/db.go:37](services/cmd/account/db.go:37),
[services/cmd/wallet/db.go:93](services/cmd/wallet/db.go:93) — все четыре
`NewDatabase*` при ошибке подключения возвращают `nil` интерфейса. Сервис
стартует и падает с nil-разыменованием на первом же запросе, вместо того чтобы
не подняться вообще.

### 4.26. 🟡 `LogColor` проверяется на непустоту, а не на значение
[services/logging.go:31](services/logging.go:31)

```go
} else if LogColor != "" {
```

`LOG_COLOR=false` включит цветной вывод. Нужен `strconv.ParseBool`.

### 4.27. 🟡 Неиспользуемое поле `cache`
[services/cmd/wallet/main.go:48](services/cmd/wallet/main.go:48) — Redis-кеш
создаётся, ошибка подключения игнорируется (`cache, _ :=`), поле кладётся
в структуру сервиса и нигде не читается. Redis при этом объявлен зависимостью
в docker-compose и держится в памяти зря.

### 4.28. 🟡 Опечатки и англицизмы в идентификаторах
`mothPaymentCalculation` (month), `AlreadyPayed` / `LastPayedAt` (paid),
`summ`, `trans_c`, `dbt_value`. Мелочь, но она уже разошлась по JSON-тегам
и колонкам БД — исправлять позже будет дороже.

---

## 5. Схема БД и SQL

### 5.1. 🔴 `down`-миграция кошелька удалит таблицы в неверном порядке
[services/cmd/wallet/migrations/1_init_schema.down.sql](services/cmd/wallet/migrations/1_init_schema.down.sql)

```sql
DROP TABLE wallet;
DROP TABLE transaction;
```

`transaction` ссылается на `wallet` внешними ключами — первый `DROP` упадёт.
Порядок нужно инвертировать (или использовать `CASCADE`).

### 5.2. 🟠 Отсутствующие `down`-миграции
Для `account/1_init_schema` и `wallet/2_change_transaction_partitions` нет
`.down.sql`. Откат невозможен, что делает `migrate down` в проде опасным.

### 5.3. 🟠 Партиционирование транзакций закомментировано целиком
[services/cmd/wallet/migrations/2_change_transaction_partitions.up.sql](services/cmd/wallet/migrations/2_change_transaction_partitions.up.sql) —
файл из 40 строк, все закомментированы. Требование № 6 в
[services/cmd/wallet/README.md:10](services/cmd/wallet/README.md:10)
(«партиционирование по месяцу создания») не выполнено, а миграция-пустышка
занимает номер версии и создаёт ложное впечатление, что оно есть.

### 5.4. 🟠 Триггер баланса не защищает от ухода в минус
[services/cmd/wallet/migrations/1_init_schema.up.sql:30-48](services/cmd/wallet/migrations/1_init_schema.up.sql:30)

- Нет проверки `balance >= 0` — списание с пустого кошелька пройдёт.
- Операция `SWAP` разрешена `CHECK`-ограничением, но в триггере не обрабатывается —
  перевод между кошельками не изменит ни одного баланса.
- Поле `source` не участвует в изменении балансов вообще.
- Нет ограничения `value > 0` — транзакция на отрицательную сумму инвертирует смысл
  операции (DEBIT с `value = -100` списывает средства в обход всех проверок).
- Резервы (`state = 'RESERVED'`) считаются в отчёте, но нигде не уменьшают доступный
  баланс на уровне БД.

Рекомендация: `CHECK (value > 0)`, `CHECK (balance >= 0)` на `wallet`, обработка
`SWAP` и `source`, и перевод логики в хранимую процедуру с явной блокировкой строк
(`SELECT ... FOR UPDATE`) — иначе конкурентные списания дадут потерянное обновление.

### 5.5. 🟠 `payment` без первичного ключа
[services/cmd/credit/migrations/1_init_schema.up.sql:28-39](services/cmd/credit/migrations/1_init_schema.up.sql:28) —
`id SERIAL NOT NULL` без `PRIMARY KEY`. Дубликаты возможны, индекса нет,
ссылаться на строку нечем.

### 5.6. 🟠 Сомнительное ограничение `UNIQUE (user_id, type)` на кредитах
Пользователь физически не сможет взять второй потребительский кредит. Для
`wallet` такое ограничение оправдано (одно на тип), для `credit` — почти наверняка
ошибка моделирования.

### 5.7. 🟡 `credit_archive` создан через `CREATE TABLE AS ... WITH NO DATA`
Такая таблица не наследует ни ограничения, ни значения по умолчанию, ни индексы —
только типы колонок. `CHECK` добавляется отдельно, но `NOT NULL` и дефолты потеряны.
Механизма переноса записей в архив тоже нет.

### 5.8. 🟡 Отсутствуют индексы
Ни одного `CREATE INDEX` во всём проекте. Заведомо нужны:
`transaction(target)`, `transaction(source)`, `transaction(created_at)`,
`payment(credit_id)`, `credit(user_id)`, `oauth_tokens(request_id)`,
`oauth_tokens(expires_at)` (для очистки).

### 5.9. 🟡 Деньги: `BIGINT` в БД против `uint` в Go
Комментарий в миграции обещает «копейки + два порядка», но
[services/shared/credit/models.go:27](services/shared/credit/models.go:27)
объявляет `Balance uint`, а расчёт в
[payments.go:35](services/cmd/credit/payments.go:35) идёт через `float64`
с усечением `uint(need)` (не округлением). Для финансовых расчётов нужен
целочисленный тип фиксированного размера (`int64` в минимальных единицах)
и банковское округление. Также `Percent uint` не позволяет задать ставку 12,5 %.

### 5.10. 🟡 `TIMESTAMP` без часового пояса
Все временные колонки — `TIMESTAMP`, а не `TIMESTAMPTZ`. В сочетании
с отсутствием `tzdata` в scratch-образе (п. 3.3) это гарантированный источник
расхождений при развёртывании в другом регионе.

### 5.11. 🟡 Нет очистки просроченных токенов
`oauth_tokens` растёт бесконечно: `getTokenSession` проверяет `expires_at`
уже после выборки ([db.go:249](services/cmd/users/db.go:249)), но удаления
просроченных записей нет нигде.

---

## 6. Безопасность

### 6.1. 🔴 CRITICAL — полный обход аутентификации через заголовок
[services/authorization.go:17-26](services/authorization.go:17)

```go
var userId = request.Header.Get("X-Authorized-Id")
```

Сервис доверяет заголовку, который приходит от клиента. Модель безопасности
предполагает, что заголовок проставляет API Gateway после валидации JWT и вырезает
пришедший снаружи. Но:

1. В [config/krakend/krakend.json](config/krakend/krakend.json) **нет ни одного
   маршрута** к сервисам `users`/`credit`/`account`/`wallet` — это неизменённый
   демо-конфиг KrakenD Playground (GitHub API, CoinGecko, Star Wars GraphQL).
   Единственный защищённый эндпоинт `/private/moderate` ходит в `/user/1.json`
   на несуществующий `fake_api`.
2. Даже если бы маршруты были — KrakenD прокидывает claim `sub` в заголовок `X-User`
   ([krakend.json:294-298](config/krakend/krakend.json:294)), а сервисы читают
   `X-Authorized-Id`. Имена не совпадают.
3. Порты всех сервисов опубликованы напрямую на хост
   ([docker-compose.yml:149,167,185,203](docker-compose.yml:149)), в обход шлюза.

**Итог:** `curl -H "X-Authorized-Id: <любой-uuid>" http://localhost:51052/credit`
создаёт кредит от имени произвольного пользователя. То же для `account` и `wallet`
(через gRPC-метаданные, [authorization.go:31](services/authorization.go:31)).

**Исправление:** не публиковать порты сервисов наружу; описать реальные маршруты
в KrakenD с `auth/validator`; согласовать имена заголовков; на стороне сервисов
дополнительно проверять подпись JWT, а не голый идентификатор.

### 6.2. 🔴 CRITICAL — незащищённые эндпоинты ротации ключей и отзыва токенов
[services/cmd/users/handlers.go:15-16](services/cmd/users/handlers.go:15)

```go
mux.HandleFunc("/rotate-keys", service.handleRotateKeys)
mux.HandleFunc("/revoke", service.handleRevoke)
```

`/rotate-keys` доступен анонимно любым методом. Один `curl` в цикле — и все выданные
токены становятся непроверяемыми (плюс генерация RSA-2048 на каждый запрос — это
готовый вектор CPU-DoS). Требуется административная аутентификация и ограничение метода.

### 6.3. 🔴 `GlobalSecret` генерируется заново при каждом старте
[services/cmd/users/service.go:30-37](services/cmd/users/service.go:30)

```go
var secret = make([]byte, 32)
_, _ = rand.Read(secret)
```

Последствия: (а) все HMAC-токены становятся невалидны при рестарте; (б) горизонтальное
масштабирование невозможно — два инстанса не проверят токены друг друга; (в) ошибка
`rand.Read` игнорируется. Секрет должен приходить из окружения/секрет-хранилища.

### 6.4. 🔴 `SendDebugMessagesToClients: true`
[services/cmd/users/service.go:36](services/cmd/users/service.go:36) — внутренние
детали ошибок fosite (включая SQL-сообщения) уходят клиенту. То же на шлюзе:
`"operation_debug": true` ([krakend.json:293](config/krakend/krakend.json:293)) и
`"level": "DEBUG"` ([krakend.json:392](config/krakend/krakend.json:392)).

### 6.5. 🔴 Приватные RSA-ключи лежат в БД открытым текстом
[services/cmd/users/db.go:107-114](services/cmd/users/db.go:107) — `MarshalPKCS1PrivateKey`
без шифрования, в обычную колонку `BYTEA`. Компрометация дампа БД = компрометация
подписи всех токенов. Минимум — шифрование ключом из KMS/Vault; лучше — вынести
подпись в Keycloak (что и предполагает README).

### 6.6. 🔴 Секреты в репозитории
| Файл | Что |
|---|---|
| [docker-compose.yml:29-31](docker-compose.yml:29) | пароли InfluxDB `pas5w0rd`, `supersecretpassword` |
| [docker-compose.yml:100-105](docker-compose.yml:100) | Keycloak `admin`/`admin`, пароль БД |
| [docker-compose.yml:120](docker-compose.yml:120) | `POSTGRES_PASSWORD: postgres` |
| [config/grafana/datasources/all.yml](config/grafana/datasources/all.yml) | пароль InfluxDB в открытом виде |
| [services/cmd/users/migrations/1_init_schema.up.sql](services/cmd/users/migrations/1_init_schema.up.sql) | `test-client` / `test-secret` |
| [_requests/http-client.env.json](_requests/http-client.env.json) | логин и пароль тестового пользователя |

Даже для локального стенда стоит перевести на `.env` (в `.gitignore`) + `env_file`,
иначе привычка утечёт в реальную конфигурацию.

### 6.7. 🔴 Ни одного TLS-соединения
- gRPC-сервер без `credentials` ([wallet/main.go:47](services/cmd/wallet/main.go:47)).
- Все HTTP — plaintext.
- PostgreSQL: `sslmode=disable` во всех четырёх DSN.
- Redis без пароля и без TLS.
- Elasticsearch: `xpack.security.enabled=false` + порт `19200` наружу
  ([docker-compose.yml:43-48](docker-compose.yml:43)).

### 6.8. 🟠 Prometheus с включённым admin API
[docker-compose.yml:86](docker-compose.yml:86) — `--web.enable-admin-api`
позволяет удалять серии и снапшотить TSDB без аутентификации, порт `9099` открыт.

### 6.9. 🟠 CORS `allow_origins: ["*"]`
[config/krakend/krakend.json:415-418](config/krakend/krakend.json:415) — в связке
с заголовком `Authorization` в `allow_headers` это разрешает любому сайту дёргать API
от имени пользователя (если токен попадёт в JS).

### 6.10. 🟠 Нет защиты от перебора
`/register` и `/auth` без rate-limit, без CAPTCHA, без блокировки после N попыток.
На шлюзе `qos/ratelimit` настроен только на демо-эндпоинт `/shop`. В Keycloak-реалме
`"bruteForceProtected": false` ([config/keycloak/realms/krakend-realm.json](config/keycloak/realms/krakend-realm.json)),
там же `"registrationAllowed": true` и `"sslRequired": "external"`.

### 6.11. 🟠 Нет политики паролей
[services/cmd/users/service.go:88-94](services/cmd/users/service.go:88) — проверяется
только непустота. Ни минимальной длины, ни проверки по спискам скомпрометированных.
bcrypt с `DefaultCost` (10) — приемлемо, но для 2026 года стоит рассмотреть cost 12
или argon2id.

### 6.12. 🟠 Нет ограничения размера тела запроса
`json.NewDecoder(r.Body).Decode(...)` в
[credit/handlers.go:69](services/cmd/credit/handlers.go:69) и
[account/handlers.go:44](services/cmd/account/handlers.go:44) читает неограниченный
объём. Нужен `http.MaxBytesReader` и `decoder.DisallowUnknownFields()`.

### 6.13. 🟠 `http.ListenAndServe` без таймаутов
Во всех трёх HTTP-сервисах используется пакетный сервер по умолчанию: нет
`ReadHeaderTimeout`, `ReadTimeout`, `WriteTimeout`, `IdleTimeout` — классический
Slowloris (`gosec G114`). Нужен явный `&http.Server{...}`.

### 6.14. 🟠 `statsviz` открыт на порту метрик
[services/logging.go:52](services/logging.go:52) — публикует детальную информацию
о рантайме (heap, горутины, GC) без аутентификации на `:8081` во всех четырёх
сервисах. Для отладки годится, для прода — утечка внутреннего состояния.
Побочный эффект: при локальном запуске всех четырёх бинарников через
[build.sh](build.sh) они конфликтуют за один и тот же порт, а ошибка `ListenAndServe`
проглатывается через `_ =` ([logging.go:58](services/logging.go:58)).

### 6.15. 🟡 `/metrics` доступен снаружи без ограничений
[credit/handlers.go:56](services/cmd/credit/handlers.go:56),
[account/handlers.go:31](services/cmd/account/handlers.go:31) — метрики отдаются
на том же публичном порту, что и бизнес-API.

### 6.16. 🟡 Логирование персональных данных
[services/cmd/users/service.go:106](services/cmd/users/service.go:106) — `username`
пишется в лог при ошибке, оттуда уходит в Elasticsearch без TTL и без маскирования.

### 6.17. 🟡 Bind-mount данных PostgreSQL внутрь репозитория
[docker-compose.yml:122](docker-compose.yml:122) — `./.pg:/var/lib/postgresql/data`.
Каталог в `.gitignore`, но на macOS/Windows это заметно медленнее и чревато
повреждением данных. Лучше именованный volume.

---

## 7. Что не реализовано (сопоставление с README)

`README.md` описывает довольно объёмную систему. Фактическое покрытие:

| Требование README | Статус |
|---|---|
| Регистрация пользователя | 🟡 частично (только username + пароль) |
| Профиль: аватар, e-mail, телефон, пол | ❌ модель `User` содержит 3 поля |
| Регистрация через соцсети / ЕСИА / маркетплейсы | ❌ |
| JWT + OAuth2 инфраструктура | 🟡 каркас на fosite, нерабочий (см. §4.2–4.8) |
| Keycloak как рекомендованный вариант | 🟡 поднят в compose, realm импортируется, но сервисами не используется |
| Интеграция с OZON | ❌ |
| Интеграция с WB | ❌ |
| Платёжные сервисы, СБП, вывод средств | ❌ |
| Оповещения (Telegram-бот, WebSocket, long-polling) | ❌ |
| Список желаний (все 8 пунктов) | ❌ сервиса нет |
| Котёл: создание, участники, взносы, статусы | ❌ сервиса нет |
| Котёл подарков (рандом, набор подарков) | ❌ |
| Котёл удачи | ❌ |
| Шопоголик | ❌ |
| GatewayAPI KrakenD | 🟡 запущен с демо-конфигом, к сервисам не подключён |
| gRPC-взаимодействие между сервисами | 🟡 один сервис, один метод, ни одного клиента |
| Кошелёк: баланс | ✅ чтение |
| Кошелёк: списание/начисление | ❌ только SQL-триггер, API нет |
| Кошелёк: перевод между кошельками | ❌ (`SWAP` не обрабатывается) |
| Кошелёк: транзакционность, синхронный/асинхронный режим | ❌ |
| Кошелёк: история операций | 🟡 таблица есть, API нет |
| Кошелёк: партиционирование по месяцам | ❌ миграция закомментирована |
| Кошелёк: статусная модель ACTIVE/BLOCKED/DELETED/CLOSED | 🟡 `CHECK` в схеме, переходов нет |
| Кошелёк: автосоздание при отсутствии | ✅ (с оговорками §4.14) |
| Сервис `account` | ❌ все методы — `panic("implement me")` |
| Сервис `credit`: создание | ✅ |
| Сервис `credit`: график платежей | 🟡 только аннуитет, на неполных данных (§4.12) |
| Сервис `credit`: дифференцированные платежи (`DYN`) | ❌ [payments.go:48](services/cmd/credit/payments.go:48) |
| Сервис `credit`: приём платежей, смена статусов, архивация | ❌ |

Три из четырёх сервисов не имеют ни одного теста. Нет ни интеграционных тестов,
ни контрактных тестов gRPC, ни нагрузочных сценариев — притом что цель № 5 в README
прямо заявляет оптимизацию по скорости и отказоустойчивости.

---

## 8. Реализовано не так

### 8.1. Два параллельных решения одной задачи — аутентификации
README прямо пишет «Лучше использовать `Keycloak`», Keycloak поднят в compose,
realm `krakend` импортируется, KrakenD настроен на его JWKS. И одновременно
написан **свой** OAuth2-провайдер на fosite с собственным управлением RSA-ключами,
собственным хранилищем токенов и собственным JWKS-эндпоинтом.

Это удвоение поверхности атаки и удвоение сопровождения. Нужно выбрать одно:
либо Keycloak (тогда `users` сводится к профилю пользователя), либо свой провайдер
(тогда Keycloak убирается из compose). Собственная реализация OAuth2 — та задача,
где ошибки стоят дороже всего, а в текущем виде она содержит как минимум
шесть блокирующих дефектов.

### 8.2. Модель авторизации построена на доверии заголовку
Даже при работающем шлюзе передача голого UUID вместо проверяемого токена означает,
что любой доступ к внутренней сети = полный доступ к API от имени любого
пользователя. Zero-trust здесь нарушается на самом базовом уровне. Правильнее —
пробрасывать сам JWT и валидировать его в сервисе (кешируя JWKS), либо использовать
mTLS между шлюзом и сервисами.

### 8.3. Бизнес-логика в SQL-триггере
[wallet/migrations/1_init_schema.up.sql:30](services/cmd/wallet/migrations/1_init_schema.up.sql:30) —
изменение баланса спрятано в триггере `AFTER UPDATE`. Это делает поведение невидимым
из кода, нетестируемым модульно и несогласуемым с бизнес-правилами (нет проверок
на отрицательный баланс, нет `SWAP`). Для денежных операций лучше явная транзакция
в коде с `SELECT ... FOR UPDATE`, либо целиком хранимая процедура с полным набором
проверок — но не половина логики в триггере.

### 8.4. Валидация — формальность
[services/shared/account/models.go:14](services/shared/account/models.go:14) —
`Validate()` возвращает `true` безусловно. В
[credit/models.go:46](services/shared/credit/models.go:46) правила выглядят
случайными: `Percent >= 10` (нельзя выдать кредит под 5 %), `Balance > 100`
(в копейках это рубль), `Month > 1`. Верхних границ нет вообще — `month: 100000`
пройдёт валидацию и попадёт в `make()` из §4.11. Теги `default:"..."` в структурах
ничего не делают: `encoding/json` их не понимает, значения по умолчанию нужно
проставлять явно.

### 8.5. Дублирование `main.go`
Четыре файла `main.go` совпадают почти дословно (recover, ExitHandle, flag.Parse,
DefineLogging, DefineMetrics, godotenv). Это ~40 строк × 4 копии, в которых уже
продублированы ошибки §4.1, §4.17 и §4.18 — исправлять придётся в четырёх местах.
Просится `services.Bootstrap(name string, run func(ctx) error)`.

### 8.6. Обработка ошибок «залогировать и продолжить»
Сквозной антипаттерн: `slog.Error(...)` без `return`, `_ = err`, `if err != nil { return nil }`.
В результате сервис поднимается в заведомо нерабочем состоянии вместо честного
падения на старте. Для микросервиса под оркестратором fail-fast всегда лучше:
контейнер перезапустится, а не будет молча отдавать 500.

### 8.7. `recover()` в `main` создаёт ложное чувство защищённости
[users/main.go:22-28](services/cmd/users/main.go:22) и аналоги. `defer recover()`
в `main` не перехватывает паники в горутинах HTTP-обработчиков — там работает
собственный recover `net/http`. Реальная защита — middleware с recover на mux
и `grpc_recovery` интерсептор для gRPC. Ни того, ни другого нет.

### 8.8. Наблюдаемость собрана, но не подключена
- Prometheus скрейпит только `credit` и `account`
  ([config/prometheus/prometheus.yml](config/prometheus/prometheus.yml));
  у `users` и `wallet` эндпоинта `/metrics` нет вообще.
- `scrape_interval: 1s` для бизнес-сервисов — избыточно агрессивно (в 5–15 раз чаще нормы).
- Единственная метрика на сервис — счётчик вызовов `Create`. Нет гистограмм
  latency, нет счётчиков ошибок, нет метрик пула БД. По таким данным ни один
  из вопросов «система тормозит?» не решается.
- OpenTelemetry подключён только в Redis-клиенте
  ([services/cache_redis.go:36](services/cache_redis.go:36)), но `TracerProvider`
  нигде не инициализируется — трейсы никуда не уходят. Jaeger в compose получает
  данные только от KrakenD.
- Трассировка не проходит через gRPC (нет `otelgrpc` интерсептора) и через HTTP.
- `filebeat` закомментирован в compose ([docker-compose.yml:68-75](docker-compose.yml:68)),
  а `LOG_FILE` закомментирован у всех сервисов — то есть цепочка
  «сервис → файл → filebeat → logstash → elasticsearch» разорвана в двух местах.
  Логи сервисов в Kibana не попадают.
- В `logstash.conf` `document_id => "%{[parsed][uuid]}"`, но `slog` поля `uuid`
  не пишет — все записи получат литеральный id `%{[parsed][uuid]}` и будут
  перезаписывать друг друга.

### 8.9. Расхождение API и коллекции запросов
[_requests/credit.http](_requests/credit.http) обращается к
`GET /credit/{id}/need_payments`, а сервис регистрирует
`GET /credits/{id}/schedule` ([handlers.go:31](services/cmd/credit/handlers.go:31)).
Запрос вернёт 404. Аналогично [_requests/wallet.http](_requests/wallet.http)
передаёт `X-User-Id`, тогда как код читает `x-authorized-id`.

### 8.10. Тест зависит от порядка выполнения подтестов
[services/cmd/credit/payments_test.go](services/cmd/credit/payments_test.go) —
второй `t.Run` мутирует ту же переменную `c`, что использовал первый.
Запуск с `-run` по отдельности или добавление `t.Parallel()` сломает тест.
Ожидаемые значения (`21247`, `21171`) захардкожены без пояснения формулы,
дельта не используется — при переходе на корректное округление тест придётся
переписывать вслепую.

---

## 9. Предложения и улучшения

### 9.1. Блокирующее (сделать до любой новой функциональности)

1. **Закрыть обход аутентификации** (§6.1): убрать `ports:` у четырёх сервисов
   из compose, описать реальные маршруты в KrakenD, валидировать JWT в сервисах.
2. **Починить IDOR в кошельке** (§4.9) — одна строка сравнения.
3. **Исправить SQL с ключами** (§4.2, §4.3, §4.5) — три запроса.
4. **Загружать `.env` до инициализации пакета `services`** (§4.1).
5. **Добавить `return` после каждого `w.WriteHeader(4xx)`** (§4.10).
6. **Закрыть `/rotate-keys` и `/revoke` административной авторизацией** (§6.2).
7. **Вынести `GlobalSecret` в окружение** (§6.3) и выключить
   `SendDebugMessagesToClients`.
8. **Определиться: Keycloak или свой fosite** (§8.1) — и удалить лишнее.

### 9.2. Инфраструктура разработки

9. Добавить **CI** (`.github/workflows/ci.yml`): `go build`, `go vet`,
   `go test -race -cover`, `gofmt -l`, `golangci-lint`, `go mod tidy -diff`,
   `govulncheck`, `gosec`, `hadolint` для Dockerfile, `trivy` для образов.
10. Добавить `.golangci.yml` с набором `errcheck`, `staticcheck`, `gosec`,
    `bodyclose`, `sqlclosecheck`, `rowserrcheck`, `contextcheck`, `nilerr`.
    `errcheck` в одиночку найдёт большинство дефектов из §4.
11. Заменить `build.sh`/`build.cmd` на `Makefile` с целями
    `proto`, `build`, `test`, `lint`, `up`, `down`, `migrate`.
12. Добавить `.dockerignore` (сейчас его нет — в контекст сборки уходят `.git`,
    `.pg`, `.logs`).
13. Убрать `go mod tidy` из Dockerfile, добавить `ca-certificates`, `tzdata`,
    `USER`, `HEALTHCHECK`.
14. Закрепить версии образов в compose (`postgres`, `grafana/grafana`,
    `prom/prometheus`, `redis:latest`, `krakend:latest` — все без тега или с `latest`).

### 9.3. Код

15. Вынести общий bootstrap сервиса в `services` (§8.5).
16. Ввести `Bootstrap`-контракт с честным graceful shutdown: `signal.NotifyContext`
    → `srv.Shutdown(ctx)` / `grpcServer.GracefulStop()` → `db.Close()` (§4.17).
17. Все конструкторы `New*` должны возвращать `(T, error)` и приводить к
    завершению процесса на старте (§4.25).
18. Перевести все обращения к БД на `...Context` (§4.21), настроить пул (§4.20).
19. Ввести слой репозитория с интерфейсами и тестами на `sqlmock`/`testcontainers` —
    сейчас SQL-ошибки ловятся только в проде.
20. Заменить деньги на `int64` в минимальных единицах, ставку — на базисные пункты
    (`int` bp), добавить явное округление (§5.9).
21. Добавить middleware: recover, request-id, structured access log, timeout,
    `MaxBytesReader`. Для gRPC — соответствующие интерсепторы.
22. Ввести валидацию на базе `go-playground/validator` вместо самописных
    `Validate() bool` без причины отказа (§8.4) — клиент должен получать
    **что именно** невалидно.
23. Убрать `goto` из [wallet/db.go](services/cmd/wallet/db.go), заменить на
    `INSERT ... ON CONFLICT DO NOTHING RETURNING` (§4.14).

### 9.4. Данные

24. Индексы (§5.8), первичный ключ на `payment` (§5.5), `TIMESTAMPTZ` (§5.10).
25. Реализовать партиционирование транзакций либо удалить пустую миграцию,
    чтобы не занимала версию (§5.3).
26. Добавить `CHECK (value > 0)` и `CHECK (balance >= 0)`, обработать `SWAP`
    и `source` (§5.4).
27. Задача очистки просроченных `oauth_tokens` (§5.11).
28. Проверить необходимость `UNIQUE (user_id, type)` на `credit` (§5.6).

### 9.5. Наблюдаемость

29. `/metrics` во всех четырёх сервисах, на отдельном служебном порту.
30. RED-метрики (Rate, Errors, Duration) на каждый эндпоинт + метрики
    `database/sql` через `sql.DBStats`.
31. Инициализировать OTel `TracerProvider` с экспортом в Jaeger, добавить
    `otelhttp` и `otelgrpc` — иначе Redis-инструментация бесполезна (§8.8).
32. Починить или удалить цепочку логов через filebeat (§8.8); добавить в `slog`
    поле `uuid`/`trace_id`, на которое рассчитывает logstash.
33. `scrape_interval` вернуть к 5–15 с.

### 9.6. Тесты

34. Тесты на `HttpAuthorized`/`GrpcAuthorized` — это точка входа безопасности,
    и она не покрыта ни одним тестом.
35. Табличные тесты на `mothPaymentCalculation` с граничными случаями:
    `Month == 0`, `Percent == 0`, `alreadyPaidMonth > Month`, огромные значения.
36. Интеграционные тесты на `testcontainers-go` с реальным PostgreSQL —
    именно они поймали бы §4.2, §4.3, §4.5, §5.1 мгновенно.
37. Контрактный тест gRPC-сервиса кошелька.
38. Убрать зависимость подтестов от порядка (§8.10).

### 9.7. Документация

39. `README` описывает целевую систему, но не текущее состояние. Стоит добавить
    раздел «Что уже работает / что в планах» и инструкцию запуска
    (сейчас неочевидно, что нужен `docker compose up postgres-db` до `go run`).
40. `ADR` на ключевые развилки: Keycloak vs собственный OAuth2, gRPC vs REST
    между сервисами, модель денег, стратегия партиционирования.
41. Дописать `Makefile`-цель `save-keycloak-config`, на которую ссылается
    [config/keycloak/README.md](config/keycloak/README.md).

---

## 10. Быстрые победы (< 1 часа суммарно, максимальный эффект)

| # | Изменение | Файл | Эффект |
|---|---|---|---|
| 1 | `WHERE id` → `WHERE key_id` | [users/db.go:100](services/cmd/users/db.go:100) | Оживляет подпись токенов |
| 2 | `rowid` → `key_id` | [users/db.go:118-119](services/cmd/users/db.go:118) | Оживляет ротацию ключей |
| 3 | `signature = ?` → `signature = $1`, `token_type = $2` | [users/db.go:258](services/cmd/users/db.go:258) | Оживляет отзыв токенов |
| 4 | Сравнить `request.UserId` с `authorized.Id` | [wallet/service.go:27](services/cmd/wallet/service.go:27) | Закрывает IDOR |
| 5 | `return` после `WriteHeader(400)` ×4 | [credit/handlers.go:73,77](services/cmd/credit/handlers.go:73), [account/handlers.go:48,52](services/cmd/account/handlers.go:48) | Останавливает запись мусора в БД |
| 6 | `fmt.Sprintf("%d", id)` → `id.String()` | [account/handlers.go:61](services/cmd/account/handlers.go:61) | Корректный `X-Account-Id` |
| 7 | `GROUP BY t.value, wlt.id` → `GROUP BY wlt.id` (×2) | [wallet/db.go:38,44](services/cmd/wallet/db.go:38) | Убирает дубли кошельков |
| 8 | Поменять местами `DROP TABLE` | [wallet/1_init_schema.down.sql](services/cmd/wallet/migrations/1_init_schema.down.sql) | Делает откат возможным |
| 9 | Убрать `ports:` у четырёх сервисов | [docker-compose.yml](docker-compose.yml) | Закрывает обход шлюза |
| 10 | `SendDebugMessagesToClients: false` | [users/service.go:36](services/cmd/users/service.go:36) | Прекращает утечку деталей ошибок |
| 11 | Убрать `RUN go mod tidy` ×4 | `services/cmd/*/Dockerfile` | Детерминированная сборка |
| 12 | Добавить `SELECT already_payed, created_at, last_payed_at` | [credit/db.go:31](services/cmd/credit/db.go:31) | Корректный график платежей |

---

## 11. Итоговая оценка

**Сильные стороны.** Аккуратная структура каталогов (`cmd`/`shared`/общий пакет),
разделение схем БД по сервисам, встроенные миграции через `embed.FS`,
последовательное использование `log/slog` со структурированными полями,
довольно полный стенд наблюдаемости, HTTP-коллекции для ручной проверки,
чистый `gofmt` и `go vet`. Для проекта, заявленного как учебный, набор охваченных
технологий (gRPC, protobuf, OAuth2, миграции, партиционирование, ELK, Prometheus)
явно шире среднего.

**Слабые стороны.** Компилируемость подменяет собой работоспособность: почти все
дефекты живут в строковых SQL-литералах, в игнорируемых ошибках и в порядке
инициализации — то есть ровно там, куда компилятор не смотрит. Отсутствие CI,
линтера и интеграционных тестов означает, что ни один из перечисленных
двенадцати «быстрых» дефектов не был бы обнаружен автоматически.

**Приоритет № 1** — не новая функциональность, а §9.1 и §9.2: закрыть обход
аутентификации, починить двенадцать однострочников из §10 и поставить CI
с линтером. После этого каждая следующая фича будет обходиться дешевле,
а не дороже.
