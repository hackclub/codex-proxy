package token

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestRefreshForm(t *testing.T) {
	expires := time.Now().Add(time.Hour).Unix()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
			t.Fatalf("content type = %q", r.Header.Get("Content-Type"))
		}
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if r.FormValue("refresh_token") != "refresh-old" {
			t.Fatalf("refresh token = %q", r.FormValue("refresh_token"))
		}
		_ = json.NewEncoder(w).Encode(refreshResponse{
			AccessToken:  jwt(map[string]any{"exp": expires, "chatgpt_account_id": "acct_1"}),
			RefreshToken: "refresh-new",
			ExpiresIn:    3600,
		})
	}))
	defer server.Close()

	manager := &Manager{httpClient: server.Client(), oauthURL: server.URL, clientID: "client"}
	refreshed, err := manager.refresh(context.Background(), "refresh-old")
	if err != nil {
		t.Fatal(err)
	}
	if refreshed.RefreshToken != "refresh-new" {
		t.Fatalf("rotated refresh token = %q", refreshed.RefreshToken)
	}
	if account := extractAccountID(refreshed.AccessToken); account != "acct_1" {
		t.Fatalf("account = %q", account)
	}
}

func TestRefreshMakesOneRequest(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if r.FormValue("refresh_token") != "refresh-old" {
			t.Fatalf("refresh token = %q", r.FormValue("refresh_token"))
		}
		_ = json.NewEncoder(w).Encode(refreshResponse{AccessToken: "access", ExpiresIn: 3600})
	}))
	defer server.Close()

	manager := &Manager{httpClient: server.Client(), oauthURL: server.URL, clientID: "client"}
	if _, err := manager.refresh(context.Background(), "refresh-old"); err != nil {
		t.Fatal(err)
	}
	if requests != 1 {
		t.Fatalf("made %d requests", requests)
	}
}

func TestRefreshRejectsOversizedResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("x", (1<<20)+1)))
	}))
	defer server.Close()

	manager := &Manager{httpClient: server.Client(), oauthURL: server.URL, clientID: "client"}
	if _, err := manager.refresh(context.Background(), "refresh-old"); err == nil || !strings.Contains(err.Error(), "exceeded 1 MiB") {
		t.Fatalf("error = %v", err)
	}
}

func TestTokenExpiryUsesJWT(t *testing.T) {
	want := time.Now().Add(2 * time.Hour).Truncate(time.Second)
	got := tokenExpiry(jwt(map[string]any{"exp": want.Unix()}), 1)
	if !got.Equal(want) {
		t.Fatalf("got %s, want %s", got, want)
	}
}

func TestQuotaFromHeaders(t *testing.T) {
	reset := time.Now().Add(time.Hour).Truncate(time.Second)
	headers := http.Header{
		"X-Codex-Plan-Type":              {"plus"},
		"X-Codex-Active-Limit":           {"premium"},
		"X-Codex-Primary-Used-Percent":   {"42"},
		"X-Codex-Primary-Reset-At":       {strconv.FormatInt(reset.Unix(), 10)},
		"X-Codex-Secondary-Used-Percent": {"7.5"},
	}

	quota := quotaFromHeaders(headers)
	if quota.PlanType != "plus" || quota.ActiveLimit != "premium" {
		t.Fatalf("plan = %q, active limit = %q", quota.PlanType, quota.ActiveLimit)
	}
	if quota.PrimaryUsedPercent == nil || *quota.PrimaryUsedPercent != 42 {
		t.Fatalf("primary used = %v", quota.PrimaryUsedPercent)
	}
	if quota.PrimaryResetAt == nil || !quota.PrimaryResetAt.Equal(reset) {
		t.Fatalf("primary reset = %v", quota.PrimaryResetAt)
	}
	if quota.SecondaryUsedPercent == nil || *quota.SecondaryUsedPercent != 7.5 {
		t.Fatalf("secondary used = %v", quota.SecondaryUsedPercent)
	}
}

func TestUsageLimitReset(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	reset := now.Add(7 * 24 * time.Hour)
	body := []byte(fmt.Sprintf(`{"error":{"type":"usage_limit_reached","resets_at":%d}}`, reset.Unix()))

	got, reason := rateLimit(nil, body, now)
	if !got.Equal(reset) || reason != "usage_limit_reached" {
		t.Fatalf("got %s (%s), want %s (usage_limit_reached)", got, reason, reset)
	}
}

func TestTemporaryRateLimitUsesRetryAfter(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	got, reason := rateLimit(http.Header{"Retry-After": {"12"}}, nil, now)
	if want := now.Add(12 * time.Second); !got.Equal(want) || reason != "rate_limited" {
		t.Fatalf("got %s (%s), want %s (rate_limited)", got, reason, want)
	}
}

func jwt(claims map[string]any) string {
	header, _ := json.Marshal(map[string]string{"alg": "none"})
	payload, _ := json.Marshal(claims)
	return base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload) + ".signature"
}
