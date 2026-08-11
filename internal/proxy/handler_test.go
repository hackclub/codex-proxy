package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/hackclub/codex-proxy/internal/db"
)

type fakeTokenPool struct {
	tokens      []db.Token
	borrowed    int
	rateLimited []string
	used        []string
	retryAt     time.Time
}

func (p *fakeTokenPool) Borrow(context.Context) (db.Token, error) {
	if p.borrowed == len(p.tokens) {
		return db.Token{}, &db.NoAvailableTokensError{RetryAt: &p.retryAt}
	}
	token := p.tokens[p.borrowed]
	p.borrowed++
	return token, nil
}

func (*fakeTokenPool) ReportAuthFailure(context.Context, string, string) {}

func (p *fakeTokenPool) ReportUsage(_ context.Context, tokenID string, _ http.Header) error {
	p.used = append(p.used, tokenID)
	return nil
}

func (p *fakeTokenPool) ReportRateLimit(_ context.Context, tokenID string, _ http.Header, _ []byte) (time.Time, error) {
	p.rateLimited = append(p.rateLimited, tokenID)
	return p.retryAt, nil
}

func TestCallUpstreamFailsOverRateLimitedToken(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer exhausted" {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"type":"usage_limit_reached"}}`))
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	pool := &fakeTokenPool{
		tokens: []db.Token{
			{ID: "one", AccessToken: "exhausted"},
			{ID: "two", AccessToken: "available"},
		},
		retryAt: time.Now().Add(time.Hour),
	}
	handler := NewHandler(nil, pool, nil, HandlerConfig{UpstreamURL: upstream.URL, HTTPClient: upstream.Client()})

	token, response, err := handler.callUpstream(context.Background(), []byte(`{}`), RequestMetadata{})
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if token.ID != "two" || response.StatusCode != http.StatusOK {
		t.Fatalf("token = %q, status = %d", token.ID, response.StatusCode)
	}
	if len(pool.rateLimited) != 1 || pool.rateLimited[0] != "one" {
		t.Fatalf("rate limited = %v", pool.rateLimited)
	}
	if len(pool.used) != 1 || pool.used[0] != "two" {
		t.Fatalf("used = %v", pool.used)
	}
}

func TestCallUpstreamReturnsLastRateLimitWhenPoolIsExhausted(t *testing.T) {
	body := `{"error":{"type":"usage_limit_reached"}}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(body))
	}))
	defer upstream.Close()

	pool := &fakeTokenPool{
		tokens:  []db.Token{{ID: "one", AccessToken: "exhausted"}},
		retryAt: time.Now().Add(time.Hour),
	}
	handler := NewHandler(nil, pool, nil, HandlerConfig{UpstreamURL: upstream.URL, HTTPClient: upstream.Client()})

	_, response, err := handler.callUpstream(context.Background(), []byte(`{}`), RequestMetadata{})
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	got, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusTooManyRequests || string(got) != body {
		t.Fatalf("status = %d, body = %q", response.StatusCode, got)
	}
	if response.Header.Get("Retry-After") == "" {
		t.Fatal("missing Retry-After")
	}
}
