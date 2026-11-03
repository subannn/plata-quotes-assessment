package implementations

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"
	ht "github.com/ogen-go/ogen/http"
	"github.com/ogen-go/ogen/ogenerrors"

	"plata-quotes-assessment/api"
	"plata-quotes-assessment/internal/database"
)

var (
	// errInvalidRequest — ошибка во входных данных, отдаётся как 400.
	errInvalidRequest = errors.New("invalid request")

	// errInvalidAPIKey — неверный X-API-Key, отдаётся как 401.
	errInvalidAPIKey = errors.New("invalid api key")
)

// QuoteUpdateStore — хранилище заявок на обновление котировок.
type QuoteUpdateStore interface {
	InsertQuoteUpdate(ctx context.Context, base, quote string, idempotencyKey uuid.UUID) (uuid.UUID, error)
	GetQuoteUpdate(ctx context.Context, id uuid.UUID) (database.QuoteUpdate, error)
}

// RateStore — хранилище полученных курсов.
type RateStore interface {
	GetLatestRate(ctx context.Context, base, quote string, at *time.Time) (database.Rate, error)
}

type Service struct {
	updates QuoteUpdateStore
	rates   RateStore
	apiKey  string
	log     *slog.Logger
}

// NewService создаёт обработчик API. apiKey — ожидаемое значение X-API-Key.
func NewService(updates QuoteUpdateStore, rates RateStore, apiKey string, log *slog.Logger) *Service {
	return &Service{updates: updates, rates: rates, apiKey: apiKey, log: log}
}

// CreateQuoteUpdate implements POST /quotes/updates.
func (s *Service) CreateQuoteUpdate(ctx context.Context, req *api.CreateUpdateRequest, params api.CreateQuoteUpdateParams) (*api.CreateUpdateResponse, error) {
	if req.Base == req.Quote {
		return nil, fmt.Errorf("%w: base and quote must differ", errInvalidRequest)
	}

	id, err := s.updates.InsertQuoteUpdate(ctx, string(req.Base), string(req.Quote), params.IdempotencyKey)
	if err != nil {
		return nil, err
	}

	return &api.CreateUpdateResponse{UpdateID: id}, nil
}

// GetQuoteUpdate implements GET /quotes/updates/{id}.
func (s *Service) GetQuoteUpdate(ctx context.Context, params api.GetQuoteUpdateParams) (*api.QuoteUpdate, error) {
	u, err := s.updates.GetQuoteUpdate(ctx, params.ID)
	if err != nil {
		return nil, err
	}

	resp := &api.QuoteUpdate{
		UpdateID: u.ID,
		Base:     api.Currency(u.Base),
		Quote:    api.Currency(u.Quote),
		Status:   api.UpdateStatus(u.Status),
	}
	if u.Price != nil {
		resp.Price = api.NewOptString(u.Price.String())
	}
	if u.RateUpdatedAt != nil {
		resp.UpdatedAt = api.NewOptDateTime(*u.RateUpdatedAt)
	}
	if u.Error != nil {
		resp.Error = api.NewOptString(*u.Error)
	}

	return resp, nil
}

// GetLatestQuote implements GET /quotes/latest.
func (s *Service) GetLatestQuote(ctx context.Context, params api.GetLatestQuoteParams) (*api.Quote, error) {
	if params.Base == params.Quote {
		return nil, fmt.Errorf("%w: base and quote must differ", errInvalidRequest)
	}

	var at *time.Time
	if v, ok := params.At.Get(); ok {
		at = &v
	}

	r, err := s.rates.GetLatestRate(ctx, string(params.Base), string(params.Quote), at)
	if err != nil {
		return nil, err
	}

	return &api.Quote{
		Base:      api.Currency(r.Base),
		Quote:     api.Currency(r.Quote),
		Price:     r.Price.String(),
		UpdatedAt: r.UpdatedAt,
	}, nil
}

// HandleApiKeyAuth проверяет заголовок X-API-Key. Пустой заголовок сюда
// не доходит: ogen сам отклоняет такой запрос.
func (s *Service) HandleApiKeyAuth(ctx context.Context, operationName api.OperationName, t api.ApiKeyAuth) (context.Context, error) {
	// Сравнение за постоянное время, чтобы ключ нельзя было подобрать по времени ответа.
	if subtle.ConstantTimeCompare([]byte(t.APIKey), []byte(s.apiKey)) != 1 {
		return ctx, errInvalidAPIKey
	}
	return ctx, nil
}

// NewError преобразует ошибку хендлера в ответ Error.
func (s *Service) NewError(ctx context.Context, err error) *api.ErrorStatusCode {
	// Ошибки авторизации ogen оборачивает в SecurityError: и неверный ключ
	// (errInvalidAPIKey), и отсутствующий заголовок.
	var secErr *ogenerrors.SecurityError
	if errors.As(err, &secErr) {
		return &api.ErrorStatusCode{
			StatusCode: http.StatusUnauthorized,
			Response:   api.Error{Code: "unauthorized", Message: "missing or invalid X-API-Key"},
		}
	}

	if errors.Is(err, errInvalidRequest) {
		return &api.ErrorStatusCode{
			StatusCode: http.StatusBadRequest,
			Response:   api.Error{Code: "invalid_request", Message: err.Error()},
		}
	}

	if errors.Is(err, database.ErrNotFound) {
		return &api.ErrorStatusCode{
			StatusCode: http.StatusNotFound,
			Response:   api.Error{Code: "not_found", Message: err.Error()},
		}
	}

	if errors.Is(err, database.ErrIdempotencyConflict) {
		return &api.ErrorStatusCode{
			StatusCode: http.StatusConflict,
			Response:   api.Error{Code: "idempotency_conflict", Message: err.Error()},
		}
	}

	if errors.Is(err, ht.ErrNotImplemented) {
		return &api.ErrorStatusCode{
			StatusCode: http.StatusNotImplemented,
			Response:   api.Error{Code: "not_implemented", Message: err.Error()},
		}
	}

	// Текст внутренней ошибки (БД и т.п.) клиенту не отдаём — только в лог.
	s.log.ErrorContext(ctx, "internal error", "error", err)
	return &api.ErrorStatusCode{
		StatusCode: http.StatusInternalServerError,
		Response:   api.Error{Code: "internal", Message: "internal error"},
	}
}
