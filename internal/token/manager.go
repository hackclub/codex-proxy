package token

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/hackclub/codex-proxy/internal/db"
)

type Manager struct {
	store            *db.Store
	httpClient       *http.Client
	oauthURL         string
	clientID         string
	refreshBuffer    time.Duration
	failureThreshold int
	locks            sync.Map
}

type Config struct {
	OAuthURL         string
	ClientID         string
	RefreshBuffer    time.Duration
	FailureThreshold int
	HTTPClient       *http.Client
}

type refreshResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
	AccountID    string `json:"account_id"`
	ExpiresIn    int64  `json:"expires_in"`
}

var ErrInvalidDonation = errors.New("invalid token donation")

func NewManager(store *db.Store, config Config) *Manager {
	client := config.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	return &Manager{
		store:            store,
		httpClient:       client,
		oauthURL:         config.OAuthURL,
		clientID:         config.ClientID,
		refreshBuffer:    config.RefreshBuffer,
		failureThreshold: config.FailureThreshold,
	}
}

func (m *Manager) Donate(ctx context.Context, label, refreshToken, accountID string) (db.TokenSummary, error) {
	label = strings.TrimSpace(label)
	refreshToken = strings.TrimSpace(refreshToken)
	accountID = strings.TrimSpace(accountID)
	if label == "" || refreshToken == "" {
		return db.TokenSummary{}, fmt.Errorf("%w: label and refresh_token are required", ErrInvalidDonation)
	}

	refreshed, err := m.refresh(ctx, refreshToken)
	if err != nil {
		return db.TokenSummary{}, err
	}
	if refreshed.RefreshToken == "" {
		refreshed.RefreshToken = refreshToken
	}
	if refreshed.AccountID != "" {
		accountID = refreshed.AccountID
	}
	if accountID == "" {
		accountID = extractAccountID(refreshed.IDToken, refreshed.AccessToken)
	}
	if accountID == "" {
		return db.TokenSummary{}, fmt.Errorf("%w: refresh succeeded but no ChatGPT account ID was provided or found in the token", ErrInvalidDonation)
	}

	token := db.Token{
		Label:                label,
		AccountID:            accountID,
		AccessToken:          refreshed.AccessToken,
		RefreshToken:         refreshed.RefreshToken,
		AccessTokenExpiresAt: tokenExpiry(refreshed.AccessToken, refreshed.ExpiresIn),
	}
	return m.store.UpsertToken(ctx, token)
}

func (m *Manager) Borrow(ctx context.Context) (db.Token, error) {
	token, err := m.store.SelectToken(ctx)
	if err != nil {
		return db.Token{}, err
	}
	if !m.needsRefresh(token) {
		return token, nil
	}

	lock := m.tokenLock(token.ID)
	lock.Lock()
	defer lock.Unlock()

	token, err = m.store.GetToken(ctx, token.ID)
	if err != nil {
		return db.Token{}, err
	}
	if !m.needsRefresh(token) {
		return token, nil
	}

	refreshed, err := m.refresh(ctx, token.RefreshToken)
	if err != nil {
		_ = m.store.RecordTokenFailure(ctx, token.ID, err.Error(), m.failureThreshold)
		return db.Token{}, fmt.Errorf("refresh token %q: %w", token.Label, err)
	}
	if refreshed.RefreshToken == "" {
		refreshed.RefreshToken = token.RefreshToken
	}
	accountID := token.AccountID
	if refreshed.AccountID != "" {
		accountID = refreshed.AccountID
	} else if discovered := extractAccountID(refreshed.IDToken, refreshed.AccessToken); discovered != "" {
		accountID = discovered
	}

	token.AccessToken = refreshed.AccessToken
	token.RefreshToken = refreshed.RefreshToken
	token.AccountID = accountID
	token.AccessTokenExpiresAt = tokenExpiry(refreshed.AccessToken, refreshed.ExpiresIn)
	if err := m.store.UpdateTokenCredentials(ctx, token); err != nil {
		return db.Token{}, err
	}
	return token, nil
}

func (m *Manager) ReportAuthFailure(ctx context.Context, tokenID, message string) {
	if tokenID != "" {
		_ = m.store.MarkTokenUnhealthy(ctx, tokenID, message)
	}
}

func (m *Manager) needsRefresh(token db.Token) bool {
	return token.AccessToken == "" || time.Now().Add(m.refreshBuffer).After(token.AccessTokenExpiresAt)
}

func (m *Manager) tokenLock(id string) *sync.Mutex {
	lock, _ := m.locks.LoadOrStore(id, &sync.Mutex{})
	return lock.(*sync.Mutex)
}

func (m *Manager) refresh(ctx context.Context, refreshToken string) (refreshResponse, error) {
	form := url.Values{
		"client_id":     {m.clientID},
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
	}
	return m.doRefresh(ctx, strings.NewReader(form.Encode()))
}

func (m *Manager) doRefresh(ctx context.Context, body io.Reader) (refreshResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.oauthURL, body)
	if err != nil {
		return refreshResponse{}, fmt.Errorf("create refresh request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := m.httpClient.Do(req)
	if err != nil {
		return refreshResponse{}, fmt.Errorf("refresh request: %w", err)
	}
	defer resp.Body.Close()
	responseBody, readErr := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	if readErr != nil {
		return refreshResponse{}, fmt.Errorf("read refresh response: %w", readErr)
	}
	if len(responseBody) > 1<<20 {
		return refreshResponse{}, errors.New("refresh response exceeded 1 MiB")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		message := strings.TrimSpace(string(responseBody))
		return refreshResponse{}, fmt.Errorf("refresh failed with HTTP %d: %s", resp.StatusCode, message)
	}

	var refreshed refreshResponse
	if err := json.Unmarshal(responseBody, &refreshed); err != nil {
		return refreshResponse{}, fmt.Errorf("decode refresh response: %w", err)
	}
	if refreshed.AccessToken == "" {
		return refreshResponse{}, errors.New("refresh response did not include access_token")
	}
	return refreshed, nil
}

func tokenExpiry(accessToken string, expiresIn int64) time.Time {
	if expiry, ok := jwtExpiry(accessToken); ok {
		return expiry
	}
	if expiresIn <= 0 {
		expiresIn = 3600
	}
	return time.Now().Add(time.Duration(expiresIn) * time.Second)
}

func jwtExpiry(raw string) (time.Time, bool) {
	claims, ok := jwtClaims(raw)
	if !ok {
		return time.Time{}, false
	}
	expiry, ok := claims["exp"].(float64)
	if !ok || expiry <= 0 {
		return time.Time{}, false
	}
	return time.Unix(int64(expiry), 0), true
}

func extractAccountID(tokens ...string) string {
	for _, raw := range tokens {
		claims, ok := jwtClaims(raw)
		if !ok {
			continue
		}
		if accountID, ok := claims["chatgpt_account_id"].(string); ok && accountID != "" {
			return accountID
		}
		if auth, ok := claims["https://api.openai.com/auth"].(map[string]any); ok {
			if accountID, ok := auth["chatgpt_account_id"].(string); ok && accountID != "" {
				return accountID
			}
		}
		if organizations, ok := claims["organizations"].([]any); ok && len(organizations) > 0 {
			if organization, ok := organizations[0].(map[string]any); ok {
				if accountID, ok := organization["id"].(string); ok && accountID != "" {
					return accountID
				}
			}
		}
	}
	return ""
}

func jwtClaims(raw string) (map[string]any, bool) {
	parts := strings.Split(raw, ".")
	if len(parts) < 2 {
		return nil, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, false
	}
	var claims map[string]any
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil, false
	}
	return claims, true
}
