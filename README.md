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

### Локально

1. Поднять PostgreSQL:

   ```sh
   docker run -d --name quotes-pg \
     -e POSTGRES_PASSWORD=postgres -e POSTGRES_DB=quotes \
     -p 5432:5432 postgres:17
   ```

2. В `config.json` указать `exchange_rates.access_key` и `http.api_key`.

3. Запустить:

   ```sh
   go run ./cmd/service -config config.json
   ```

Схема БД применяется при старте. Сборка бинарника — `go build -o bin/quotes-service ./cmd/service`, тесты — `go test ./...`.

## API

Все запросы требуют заголовок `X-API-Key`. Цена передаётся строкой, чтобы не терять точность.

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

| Поле | Описание |
|---|---|
| `http.addr` | Адрес HTTP-сервера, напр. `:8080`. |
| `http.api_key` | Ключ для заголовка `X-API-Key`. |
| `database.url` | DSN PostgreSQL. |
| `exchange_rates.url` | URL провайдера; необязательное, по умолчанию `https://api.exchangeratesapi.io/v1`. |
| `exchange_rates.access_key` | Ключ exchangeratesapi.io. |
| `worker.batch_size` | Сколько заявок воркер забирает за раз. |
| `worker.poll_interval` | Как часто воркер проверяет очередь, напр. `1s`. |
| `worker.max_attempts` | Сколько раз пробовать заявку, прежде чем перевести в `failed`. |
| `worker.lock_timeout` | Через сколько заявка в `processing` считается брошенной и возвращается в очередь, напр. `1m`. |
| `worker.rate_ttl` | Сколько курс от провайдера считается свежим и переиспользуется без запроса, напр. `1m`; необязательное, `0s` — без кеша. |

Как подбирать параметры воркера — в [docs/DESIGN.md](docs/DESIGN.md#конфигурация).

После изменения `api/openapi.yaml` сервер перегенерируется командой `go generate ./api`.
