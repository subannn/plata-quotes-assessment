# Что осталось сделать

## Обязательное по заданию

- [x] **`GET /quotes/latest`** — `Service.GetLatestQuote` + `database.GetLatestRate`.
- [x] **Проверка `X-API-Key`** — `Service.HandleApiKeyAuth`; нет заголовка или неверный ключ → `401`.
- [x] **Идемпотентность `POST /quotes/updates`** — повтор с тем же ключом и парой возвращает ту же заявку (`202`), с другой парой — `409`.
- [x] **README.md** — описание, запуск, сборка, API с примерами, конфигурация.
- [x] **GitHub** — проект выложен.

## Проверка

- [x] **Прогнать сервис целиком с PostgreSQL** — проверено через Postman на Docker Compose: схема применяется при создании базы, заявка создаётся, воркер получает курс, курс отдаётся по заявке и как последний. Остановка по сигналу отдельно не проверялась.
- [x] **Интеграционные тесты** — проходят. Настройки и PostgreSQL (со схемой) — из `config.json`.
  - `cmd/test/integration_test.go` (тег `integration`) — сценарии с фейковым провайдером: обработка заявки, последний курс и `at`, идемпотентность, валидация, `X-API-Key`, ошибка провайдера и `max_attempts`, группировка по base, кеш курсов, зависшие заявки. Во время прогона на той же базе не должен работать сервис.

    ```sh
    go test -tags integration ./cmd/test -v
    ```

  - `cmd/service/main_test.go` (тег `fun`) — три вызова `Service` с настоящим провайдером: `CreateQuoteUpdate` → `GetQuoteUpdate` → `GetLatestQuote`. Запускается отдельно, чтобы не мешать тестам выше:

    ```sh
    go test -tags fun ./cmd/service -v
    ```

## Необязательное по заданию

- [ ] **Unit-тесты** — сейчас есть только `internal/exchangerates/client_test.go` (плюс интеграционные тесты выше). Не покрыты:
  - хендлеры (`implementations/handler.go`), включая маппинг ошибок в `NewError`;
  - воркер (`groupByBase`, `processGroup`) с фейковыми стором и провайдером;
  - `internal/config`.
- [x] **Docker** — `Dockerfile`, `docker-compose.yml` (сервис + PostgreSQL), `config.docker.json`.
