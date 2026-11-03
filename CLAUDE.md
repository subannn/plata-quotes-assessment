# FX Quotes Service

Асинхронный сервис котировок валют (Go, PostgreSQL, ogen) — тестовое задание
из docs/PlataAssignment.MD. Устройство сервиса — docs/DESIGN.md, запуск и API — README.md.

## Структура 

- `cmd/service` — точка входа: конфиг, HTTP-сервер, воркер, graceful shutdown
- `api/` — OpenAPI-спека (`openapi.yaml`) и сгенерированный по ней ogen-сервер
- `implementations/` — хендлеры API и фоновый воркер
- `internal/database` — SQL-запросы (заявки — `quote_updates.go`, курсы — `rates.go`)
- `internal/exchangerates` — клиент exchangeratesapi.io
- `internal/config` — загрузка и проверка JSON-конфига
- `migration/init.up.sql` — схема БД
- `cmd/test`, `cmd/service/main_test.go` — интеграционные тесты

## Команды

- Сборка: `go build ./...`
- Unit-тесты: `go test ./...`
- Сервис целиком: `docker compose up --build` (API на localhost:8085)
- Генерация ogen-сервера: `go generate ./api` (директива `//go:generate` — в `api/api.go`)
- Интеграционные тесты — нужен Postgres из compose (`docker compose up -d postgres`,
  порт 5435, его использует `config.json`); сервис из compose при этом остановить:
  - `go test -tags integration ./cmd/test -v` — сценарии с фейковым провайдером
  - `go test -tags fun ./cmd/service -v` — с настоящим провайдером, запускать отдельно

## Важно

- API описан в `api/openapi.yaml`; `api/oas_*_gen.go` руками не править —
  после изменения спеки перегенерировать: `go generate ./api`.
- Сервис сам схему БД не применяет: в compose её применяет Postgres при создании базы,
  при локальном запуске — вручную (`psql -f migration/init.up.sql`).
- База в compose хранится внутри контейнера: `docker compose down` её удаляет.
- Документация и комментарии в коде — на русском.
