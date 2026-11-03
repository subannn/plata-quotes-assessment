# FX Quotes Service

Сервис котировок валютных курсов с асинхронным API. Клиент создаёт заявку на обновление котировки и сразу получает её идентификатор; курс у [exchangeratesapi.io](https://exchangeratesapi.io/) запрашивает фоновый воркер. Результат клиент забирает по идентификатору заявки или как последний известный курс пары.

Валюты: `USD`, `EUR`, `MXN`. Устройство сервиса — [docs/DESIGN.md](docs/DESIGN.md).

**Стек:** Go 1.26, PostgreSQL 13+, [ogen](https://github.com/ogen-go/ogen) (сервер генерируется из [`api/openapi.yaml`](api/openapi.yaml)), [pgx](https://github.com/jackc/pgx).

## Запуск

### Docker Compose

Сервис и PostgreSQL одной командой. В `config.docker.json` указать `exchange_rates.access_key` (ключ exchangeratesapi.io) и `http.api_key` (ключ для клиентов API), затем:

```sh
docker compose up --build
```

Сервис — на `localhost:8085`. Конфиг монтируется в контейнер, в образ не входит.

База хранится внутри контейнера PostgreSQL: переживает `docker compose stop`/`start`, но удаляется при `docker compose down`. Схему БД (`migration/init.up.sql`) PostgreSQL применяет при создании новой базы. Сам сервис схему не применяет.

### Локально

1. Поднять PostgreSQL:

   ```sh
   docker run -d --name quotes-pg \
     -e POSTGRES_PASSWORD=postgres -e POSTGRES_DB=quotes \
     -p 5435:5432 postgres:17
   ```

2. Применить схему БД (сервис сам её не применяет):

   ```sh
   docker exec -i quotes-pg psql -U postgres -d quotes < migration/init.up.sql
   ```

3. В `config.json` указать `exchange_rates.access_key` и `http.api_key`.

4. Запустить:

   ```sh
   go run ./cmd/service -config config.json
   ```

Сборка бинарника — `go build -o bin/quotes-service ./cmd/service`.

## Тесты

Unit-тесты:

```sh
go test ./...
```

Интеграционные тесты вызывают `Service` с настоящими PostgreSQL и воркером; настройки берутся из `config.json`. Нужна база со схемой — проще всего Postgres из compose (порт `5435`); сервис из compose на время тестов остановить, иначе его воркер заберёт заявки тестов:

```sh
docker compose up -d postgres

# Сценарии с фейковым провайдером курсов
go test -tags integration ./cmd/test -v

# Создание заявки и получение курса с настоящим провайдером (запускать отдельно)
go test -tags fun ./cmd/service -v
```

## API

Все запросы требуют заголовок `X-API-Key`. Цена передаётся строкой, чтобы не терять точность.

В примерах — порт локального запуска `8080`; при запуске через Docker Compose — `8085`.

### Создать заявку

```sh
curl -X POST http://localhost:8080/quotes/updates \
  -H 'X-API-Key: change-me' \
  -H "Idempotency-Key: $(uuidgen)" \
  -H 'Content-Type: application/json' \
  -d '{"base": "EUR", "quote": "MXN"}'
```

```json
{"update_id": "6f1c2b7e-3d4a-4e8b-9c1f-2a5b6c7d8e9f"}
```

Ответ `202`; обновление выполняется в фоне. `Idempotency-Key` (UUID) обязателен: повтор с тем же ключом и той же парой возвращает ту же заявку, с другой парой — `409`.

### Получить котировку по заявке

```sh
curl http://localhost:8080/quotes/updates/6f1c2b7e-3d4a-4e8b-9c1f-2a5b6c7d8e9f \
  -H 'X-API-Key: change-me'
```

```json
{
  "update_id": "6f1c2b7e-3d4a-4e8b-9c1f-2a5b6c7d8e9f",
  "base": "EUR",
  "quote": "MXN",
  "status": "done",
  "price": "20.4512",
  "updated_at": "2026-11-02T10:15:00Z"
}
```

`status`: `pending` → `processing` → `done` (есть `price` и `updated_at` — время курса у провайдера) или `failed` (в `error` — причина).

### Получить последнюю котировку

```sh
curl 'http://localhost:8080/quotes/latest?base=EUR&quote=MXN' \
  -H 'X-API-Key: change-me'
```

```json
{"base": "EUR", "quote": "MXN", "price": "20.4512", "updated_at": "2026-11-02T10:15:00Z"}
```

С параметром `at` (RFC 3339) — последний курс на этот момент или раньше.

### Ошибки

Тело: `{"code": "...", "message": "..."}`.

| Код | Когда |
|---|---|
| `400` | Некорректный запрос: `base` = `quote`, неизвестная валюта, нет `Idempotency-Key` и т.п. |
| `401` | Нет `X-API-Key` или ключ неверный. |
| `404` | Заявка или курс не найдены. |
| `409` | `Idempotency-Key` уже использован для другой пары. |
| `500` | Внутренняя ошибка (подробности — в логах). |

Ошибки разбора запроса (нет `Idempotency-Key`, неизвестная валюта, некорректный UUID или JSON) формирует сгенерированный сервер, их тело — `{"error_message": "..."}`.

## Конфигурация

JSON-файл, путь — флаг `-config` (по умолчанию `config.json`). Неизвестные поля и пустые обязательные значения — ошибка при старте.

Пример (`config.json`; комментарии здесь только для пояснения — в настоящем файле их быть не должно):

```jsonc
{
  "http": {
    "addr": ":8080",                    // адрес HTTP-сервера
    "api_key": "change-me"              // ключ для заголовка X-API-Key
  },
  "database": {
    "url": "postgres://postgres:postgres@localhost:5435/quotes?sslmode=disable" // DSN PostgreSQL
  },
  "exchange_rates": {
    "url": "https://api.exchangeratesapi.io/v1", // URL провайдера; необязательное, это значение по умолчанию
    "access_key": "change-me"           // ключ exchangeratesapi.io
  },
  "worker": {
    "batch_size": 100,                  // сколько заявок воркер забирает за раз
    "poll_interval": "1s",              // как часто воркер проверяет очередь
    "max_attempts": 3,                  // сколько раз пробовать заявку, прежде чем перевести в failed
    "lock_timeout": "1m",               // через сколько заявка в processing считается брошенной и возвращается в очередь
    "rate_ttl": "1m"                    // сколько курс от провайдера переиспользуется без запроса; необязательное, "0s" — без кеша
  }
}
```

Длительности — строки в формате Go: `"500ms"`, `"1s"`, `"1m"`.

Как подбирать параметры воркера — в [docs/DESIGN.md](docs/DESIGN.md#конфигурация).

После изменения `api/openapi.yaml` сервер перегенерируется командой `go generate ./api`.
