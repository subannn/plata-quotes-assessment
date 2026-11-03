package exchangerates

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/shopspring/decimal"
)

// DefaultURL — базовый адрес API exchangeratesapi.io.
const DefaultURL = "https://api.exchangeratesapi.io/v1"

const (
	// priceScale совпадает с NUMERIC(24, 10) в таблице rates.
	priceScale = 10

	maxResponseBody = 1 << 20
)

// Rate — курс пары base/quote: сколько quote стоит одна единица base.
type Rate struct {
	Base      string
	Quote     string
	Price     decimal.Decimal
	UpdatedAt time.Time // время курса у провайдера
}

// Error — ошибка, которую вернул провайдер (HTTP-статус и/или тело с error).
type Error struct {
	StatusCode int
	Code       int
	Type       string
	Info       string
}

func (e *Error) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "exchangerates: provider error (http %d", e.StatusCode)
	if e.Code != 0 {
		fmt.Fprintf(&b, ", code %d", e.Code)
	}
	if e.Type != "" {
		fmt.Fprintf(&b, ", %s", e.Type)
	}
	b.WriteString(")")
	if e.Info != "" {
		fmt.Fprintf(&b, ": %s", e.Info)
	}
	return b.String()
}

// Client получает курсы валют из exchangeratesapi.io.
type Client struct {
	baseURL    string
	accessKey  string
	httpClient *http.Client
}

// NewClient создаёт клиент. Если httpClient == nil, используется
// http.Client с таймаутом 10 секунд.
func NewClient(baseURL, accessKey string, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 10 * time.Second}
	}

	return &Client{
		baseURL:    strings.TrimRight(baseURL, "/"),
		accessKey:  accessKey,
		httpClient: httpClient,
	}
}

type latestResponse struct {
	Success   bool                   `json:"success"`
	Timestamp int64                  `json:"timestamp"`
	Base      string                 `json:"base"`
	Rates     map[string]json.Number `json:"rates"`
	Error     *struct {
		Code int    `json:"code"`
		Type string `json:"type"`
		Info string `json:"info"`
	} `json:"error"`
	Message string `json:"message"`
}

// GetRates одним запросом возвращает актуальные курсы base к каждой из quotes.
// Результат проиндексирован по quote. Если провайдер не вернул курс хотя бы
// для одной валюты, возвращается ошибка.
func (c *Client) GetRates(ctx context.Context, base string, quotes ...string) (map[string]Rate, error) {
	if len(quotes) == 0 {
		return nil, errors.New("exchangerates: no quote currencies requested")
	}

	symbols := make([]string, 0, len(quotes))
	seen := make(map[string]struct{}, len(quotes))
	for _, quote := range quotes {
		if quote == base {
			return nil, fmt.Errorf("exchangerates: base and quote must differ, got %s", base)
		}
		if _, ok := seen[quote]; ok {
			continue
		}
		seen[quote] = struct{}{}
		symbols = append(symbols, quote)
	}

	resp, err := c.latest(ctx, base, symbols...)
	if err != nil {
		return nil, err
	}

	if resp.Base != base {
		return nil, fmt.Errorf("exchangerates: requested base %s, got %s", base, resp.Base)
	}

	if resp.Timestamp <= 0 {
		return nil, errors.New("exchangerates: response has no timestamp")
	}
	updatedAt := time.Unix(resp.Timestamp, 0).UTC()

	rates := make(map[string]Rate, len(symbols))
	for _, quote := range symbols {
		price, err := resp.rate(quote)
		if err != nil {
			return nil, err
		}

		rates[quote] = Rate{
			Base:      base,
			Quote:     quote,
			Price:     price,
			UpdatedAt: updatedAt,
		}
	}

	return rates, nil
}

func (c *Client) latest(ctx context.Context, base string, symbols ...string) (*latestResponse, error) {
	q := url.Values{}
	q.Set("access_key", c.accessKey)
	q.Set("base", base)
	q.Set("symbols", strings.Join(symbols, ","))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/latest?"+q.Encode(), nil)
	if err != nil {
		return nil, fmt.Errorf("exchangerates: build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	httpResp, err := c.httpClient.Do(req)
	if err != nil {
		// url.Error содержит полный URL вместе с access_key — убираем его из ошибки.
		var uerr *url.Error
		if errors.As(err, &uerr) {
			err = uerr.Err
		}
		return nil, fmt.Errorf("exchangerates: request failed: %w", err)
	}
	defer httpResp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(httpResp.Body, maxResponseBody))
	if err != nil {
		return nil, fmt.Errorf("exchangerates: read response: %w", err)
	}

	var resp latestResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		if httpResp.StatusCode != http.StatusOK {
			return nil, &Error{StatusCode: httpResp.StatusCode}
		}
		return nil, fmt.Errorf("exchangerates: decode response: %w", err)
	}

	if httpResp.StatusCode != http.StatusOK || !resp.Success {
		perr := &Error{StatusCode: httpResp.StatusCode, Info: resp.Message}
		if resp.Error != nil {
			perr.Code = resp.Error.Code
			perr.Type = resp.Error.Type
			perr.Info = resp.Error.Info
		}
		return nil, perr
	}

	return &resp, nil
}

// rate возвращает курс валюты относительно базовой валюты ответа,
// округлённый до priceScale знаков.
func (r *latestResponse) rate(currency string) (decimal.Decimal, error) {
	raw, ok := r.Rates[currency]
	if !ok {
		return decimal.Decimal{}, fmt.Errorf("exchangerates: no rate for %s in response", currency)
	}

	d, err := decimal.NewFromString(raw.String())
	if err != nil {
		return decimal.Decimal{}, fmt.Errorf("exchangerates: parse rate for %s: %w", currency, err)
	}

	d = d.Round(priceScale)
	if !d.IsPositive() {
		return decimal.Decimal{}, fmt.Errorf("exchangerates: non-positive rate for %s: %s", currency, d)
	}

	return d, nil
}
