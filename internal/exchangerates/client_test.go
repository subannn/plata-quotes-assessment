package exchangerates

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const latestOK = `{
	"success": true,
	"timestamp": 1761955200,
	"base": "EUR",
	"date": "2025-11-01",
	"rates": {"USD": 1.1, "MXN": 22.045454545454}
}`

func newTestClient(t *testing.T, status int, body string) (*Client, *http.Request) {
	t.Helper()

	var got http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = *r
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)

	return NewClient(srv.URL+"/v1/", "secret", srv.Client()), &got
}

func TestClient_GetRates(t *testing.T) {
	p, req := newTestClient(t, http.StatusOK, latestOK)

	rates, err := p.GetRates(context.Background(), "EUR", "USD", "MXN", "USD")
	if err != nil {
		t.Fatalf("GetRates: %v", err)
	}

	want := map[string]string{"USD": "1.1", "MXN": "22.0454545455"}
	if len(rates) != len(want) {
		t.Fatalf("got %d rates, want %d: %v", len(rates), len(want), rates)
	}
	updatedAt := time.Unix(1761955200, 0).UTC()
	for quote, price := range want {
		rate, ok := rates[quote]
		if !ok {
			t.Fatalf("no rate for %s", quote)
		}
		if rate.Base != "EUR" || rate.Quote != quote {
			t.Errorf("%s: pair = %s/%s", quote, rate.Base, rate.Quote)
		}
		if rate.Price.String() != price {
			t.Errorf("%s: price = %s, want %s", quote, rate.Price, price)
		}
		if !rate.UpdatedAt.Equal(updatedAt) {
			t.Errorf("%s: updated_at = %v, want %v", quote, rate.UpdatedAt, updatedAt)
		}
	}

	if req.URL.Path != "/v1/latest" {
		t.Errorf("path = %s, want /v1/latest", req.URL.Path)
	}
	q := req.URL.Query()
	if q.Get("access_key") != "secret" || q.Get("base") != "EUR" || q.Get("symbols") != "USD,MXN" {
		t.Errorf("unexpected query: %s", req.URL.RawQuery)
	}
}

func TestClient_BaseMismatch(t *testing.T) {
	p, _ := newTestClient(t, http.StatusOK, latestOK)

	if _, err := p.GetRates(context.Background(), "USD", "MXN"); err == nil {
		t.Fatal("want error when response base differs from requested")
	}
}

func TestClient_ProviderError(t *testing.T) {
	p, _ := newTestClient(t, http.StatusOK, `{
		"success": false,
		"error": {"code": 101, "type": "invalid_access_key", "info": "You have not supplied a valid API Access Key."}
	}`)

	_, err := p.GetRates(context.Background(), "USD", "MXN")

	var perr *Error
	if !errors.As(err, &perr) {
		t.Fatalf("want *Error, got %v", err)
	}
	if perr.Code != 101 || perr.Type != "invalid_access_key" {
		t.Errorf("unexpected error: %+v", perr)
	}
}

func TestClient_NonJSONErrorStatus(t *testing.T) {
	p, _ := newTestClient(t, http.StatusBadGateway, "<html>bad gateway</html>")

	_, err := p.GetRates(context.Background(), "USD", "MXN")

	var perr *Error
	if !errors.As(err, &perr) || perr.StatusCode != http.StatusBadGateway {
		t.Fatalf("want *Error with 502, got %v", err)
	}
}

func TestClient_PartialResponse(t *testing.T) {
	p, _ := newTestClient(t, http.StatusOK, `{"success": true, "timestamp": 1, "base": "EUR", "rates": {"USD": 1.1}}`)

	if _, err := p.GetRates(context.Background(), "EUR", "USD", "MXN"); err == nil {
		t.Fatal("want error when MXN rate is missing")
	}
}

func TestClient_ErrorDoesNotLeakAccessKey(t *testing.T) {
	p := NewClient("http://127.0.0.1:1", "secret", nil)

	_, err := p.GetRates(context.Background(), "USD", "MXN")
	if err == nil {
		t.Fatal("want connection error")
	}
	if strings.Contains(err.Error(), "secret") {
		t.Errorf("error leaks access key: %v", err)
	}
}

func TestClient_InvalidArgs(t *testing.T) {
	p, _ := newTestClient(t, http.StatusOK, latestOK)

	if _, err := p.GetRates(context.Background(), "USD"); err == nil {
		t.Error("want error for empty quotes")
	}
	if _, err := p.GetRates(context.Background(), "USD", "MXN", "USD"); err == nil {
		t.Error("want error when quote equals base")
	}
}

func TestClient_RealRequest(t *testing.T) {
	p, _ := newTestClient(t, http.StatusOK, latestOK)

	if _, err := p.GetRates(context.Background(), "USD"); err == nil {
		t.Error("want error for empty quotes")
	}
	if _, err := p.GetRates(context.Background(), "USD", "MXN", "USD"); err == nil {
		t.Error("want error when quote equals base")
	}
}
