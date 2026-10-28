package proxy

import (
	"context"
	"errors"
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
	refreshed   map[string]string
	reauthed    []string
	authFailed  []string
}

func (p *fakeTokenPool) Borrow(context.Context) (db.Token, error) {
	if p.borrowed == len(p.tokens) {
		return db.Token{}, &db.NoAvailableTokensError{RetryAt: &p.retryAt}
	}
	token := p.tokens[p.borrowed]
	p.borrowed++
	return token, nil
}

func (p *fakeTokenPool) Reauthenticate(_ context.Context, token db.Token) (db.Token, error) {
	p.reauthed = append(p.reauthed, token.ID)
	access, ok := p.refreshed[token.ID]
	if !ok {
		return db.Token{}, errors.New("refresh failed")
	}
	token.AccessToken = access
	return token, nil
}

func (p *fakeTokenPool) ReportAuthFailure(_ context.Context, tokenID, _ string) {
	p.authFailed = append(p.authFailed, tokenID)
}

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

func authUpstream(valid string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+valid {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
}

func TestCallUpstreamRefreshesRejectedToken(t *testing.T) {
	upstream := authUpstream("fresh")
	defer upstream.Close()

	pool := &fakeTokenPool{
		tokens:    []db.Token{{ID: "one", AccessToken: "revoked"}},
		refreshed: map[string]string{"one": "fresh"},
	}
	handler := NewHandler(nil, pool, nil, HandlerConfig{UpstreamURL: upstream.URL, HTTPClient: upstream.Client()})

	token, response, err := handler.callUpstream(context.Background(), []byte(`{}`), RequestMetadata{})
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if token.ID != "one" || token.AccessToken != "fresh" || response.StatusCode != http.StatusOK {
		t.Fatalf("token = %+v, status = %d", token, response.StatusCode)
	}
	if len(pool.authFailed) != 0 {
		t.Fatalf("auth failed = %v", pool.authFailed)
	}
}

func TestCallUpstreamFailsOverWhenRefreshFails(t *testing.T) {
	upstream := authUpstream("good")
	defer upstream.Close()

	pool := &fakeTokenPool{
		tokens: []db.Token{
			{ID: "one", AccessToken: "revoked"},
			{ID: "two", AccessToken: "good"},
		},
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
	if len(pool.reauthed) != 1 || pool.reauthed[0] != "one" {
		t.Fatalf("reauthed = %v", pool.reauthed)
	}
}

func TestCallUpstreamStopsWhenPoolReturnsRejectedToken(t *testing.T) {
	upstream := authUpstream("never")
	defer upstream.Close()

	rejected := db.Token{ID: "one", AccessToken: "revoked"}
	pool := &fakeTokenPool{
		tokens:    []db.Token{rejected, rejected},
		refreshed: map[string]string{"one": "still-revoked"},
	}
	handler := NewHandler(nil, pool, nil, HandlerConfig{UpstreamURL: upstream.URL, HTTPClient: upstream.Client()})

	_, response, err := handler.callUpstream(context.Background(), []byte(`{}`), RequestMetadata{})
	if response != nil {
		response.Body.Close()
	}
	if !errors.Is(err, db.ErrNoAvailableTokens) {
		t.Fatalf("err = %v", err)
	}
	if len(pool.authFailed) != 1 || pool.authFailed[0] != "one" {
		t.Fatalf("auth failed = %v", pool.authFailed)
	}
}
