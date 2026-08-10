package token

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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

func jwt(claims map[string]any) string {
	header, _ := json.Marshal(map[string]string{"alg": "none"})
	payload, _ := json.Marshal(claims)
	return base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload) + ".signature"
}
