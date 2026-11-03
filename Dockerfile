# Сборка.
FROM golang:1.26-alpine AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/quotes-service ./cmd/service

# Запуск. distroless/static содержит CA-сертификаты для HTTPS к провайдеру.
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/quotes-service /app/quotes-service

EXPOSE 8080

# Конфиг в образ не входит (в нём ключи) — монтируется в /app/config.json.
ENTRYPOINT ["/app/quotes-service", "-config", "/app/config.json"]
