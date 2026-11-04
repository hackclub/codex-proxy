package db

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/hackclub/codex-proxy/internal/secretbox"
)

// Needs a throwaway postgres: it wipes the tokens table.
func limitStore(t *testing.T) (*Store, context.Context) {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	pool, err := Connect(ctx, url, 4)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `TRUNCATE tokens CASCADE`); err != nil {
		t.Fatal(err)
	}
	box, err := secretbox.New("this-is-a-test-secret-with-more-than-32-characters")
	if err != nil {
		t.Fatal(err)
	}
	return NewStore(pool, box), ctx
}

func limitedToken(t *testing.T, store *Store, ctx context.Context, label, account string, lastUsedAgo time.Duration) string {
	t.Helper()
	summary, err := store.UpsertToken(ctx, Token{
		Label: label, AccountID: account, AccessToken: "access", RefreshToken: "refresh",
		AccessTokenExpiresAt: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `
		UPDATE tokens SET limited_until = now() + interval '5 days', limit_reason = 'usage_limit_reached',
			last_used_at = now() - make_interval(secs => $2)
		WHERE id = $1
	`, summary.ID, lastUsedAgo.Seconds()); err != nil {
		t.Fatal(err)
	}
	return summary.ID
}

func TestSelectTokenRechecksLimitedAccountEveryFiveMinutes(t *testing.T) {
	store, ctx := limitStore(t)
	id := limitedToken(t, store, ctx, "stale", "account", 6*time.Minute)

	token, err := store.SelectToken(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if token.ID != id {
		t.Fatalf("selected %q, want %q", token.ID, id)
	}

	_, err = store.SelectToken(ctx)
	var unavailable *NoAvailableTokensError
	if !errors.As(err, &unavailable) || unavailable.RetryAt == nil {
		t.Fatalf("second select within the window: err = %v", err)
	}
}

func TestSelectTokenRechecksOneTokenPerAccount(t *testing.T) {
	store, ctx := limitStore(t)
	limitedToken(t, store, ctx, "stale", "account", 6*time.Minute)
	limitedToken(t, store, ctx, "recent", "account", time.Minute)

	if token, err := store.SelectToken(ctx); !errors.Is(err, ErrNoAvailableTokens) {
		t.Fatalf("selected %q, err = %v", token.Label, err)
	}
}

func TestSelectTokenWaitsOutTemporaryRateLimit(t *testing.T) {
	store, ctx := limitStore(t)
	id := limitedToken(t, store, ctx, "throttled", "account", time.Hour)
	if _, err := store.pool.Exec(ctx, `UPDATE tokens SET limit_reason = 'rate_limited' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}

	if token, err := store.SelectToken(ctx); !errors.Is(err, ErrNoAvailableTokens) {
		t.Fatalf("selected %q, err = %v", token.Label, err)
	}
}

func TestGetTokenReturnsLimitedToken(t *testing.T) {
	store, ctx := limitStore(t)
	id := limitedToken(t, store, ctx, "limited", "account", time.Minute)

	if _, err := store.GetToken(ctx, id); err != nil {
		t.Fatal(err)
	}
}

func TestUpdateTokenQuotaClearLimit(t *testing.T) {
	store, ctx := limitStore(t)
	id := limitedToken(t, store, ctx, "one", "account", time.Minute)
	limitedToken(t, store, ctx, "two", "account", time.Minute)

	limits := func() int {
		var count int
		if err := store.pool.QueryRow(ctx, `SELECT count(limited_until) + count(limit_reason) FROM tokens`).Scan(&count); err != nil {
			t.Fatal(err)
		}
		return count
	}

	if err := store.UpdateTokenQuota(ctx, id, TokenQuota{}); err != nil {
		t.Fatal(err)
	}
	if got := limits(); got != 4 {
		t.Fatalf("usage without ClearLimit left %d limit columns, want 4", got)
	}

	if err := store.UpdateTokenQuota(ctx, id, TokenQuota{ClearLimit: true}); err != nil {
		t.Fatal(err)
	}
	if got := limits(); got != 0 {
		t.Fatalf("ClearLimit left %d limit columns across the account, want 0", got)
	}
}
