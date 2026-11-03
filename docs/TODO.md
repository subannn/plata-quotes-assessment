# Что осталось сделать

## Обязательное по заданию

- [x] **`GET /quotes/latest`** — `Service.GetLatestQuote` + `database.GetLatestRate`.
- [x] **Проверка `X-API-Key`** — `Service.HandleApiKeyAuth`; нет заголовка или неверный ключ → `401`.
- [x] **Идемпотентность `POST /quotes/updates`** — повтор с тем же ключом и парой возвращает ту же заявку (`202`), с другой парой — `409`.
- [x] **README.md** — описание, запуск, сборка, API с примерами, конфигурация.
- [ ] **GitHub** — проект пока не git-репозиторий.

## Проверка

- [ ] **Прогнать сервис целиком с PostgreSQL**: миграция, создание заявки, обработка воркером, получение курса, остановка по сигналу. Пока проверены только сборка и ошибки старта (нет конфига, нет БД).

## Необязательное по заданию

- [ ] **Unit-тесты** — сейчас есть только `internal/exchangerates/client_test.go`. Не покрыты:
  - хендлеры (`implementations/handler.go`), включая маппинг ошибок в `NewError`;
  - воркер (`groupByBase`, `processGroup`) с фейковыми стором и провайдером;
  - `internal/config`.
- [x] **Docker** — `Dockerfile`, `docker-compose.yml` (сервис + PostgreSQL), `config.docker.json`. Образ ещё не собирался.
