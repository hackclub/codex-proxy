package db

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

var (
	ErrInvalidAPIKey   = errors.New("invalid API key")
	ErrNoHealthyTokens = errors.New("no healthy tokens available")
	ErrNotFound        = errors.New("not found")
)

type APIKey struct {
	ID            string     `json:"id"`
	Username      string     `json:"username"`
	AppName       string     `json:"app_name"`
	MachineName   string     `json:"machine_name"`
	KeyPrefix     string     `json:"key_prefix"`
	RateLimitRPS  float64    `json:"rate_limit_rps"`
	Enabled       bool       `json:"enabled"`
	CreatedAt     time.Time  `json:"created_at"`
	LastUsedAt    *time.Time `json:"last_used_at"`
	TotalRequests int64      `json:"total_requests"`
}

func (k APIKey) Label() string {
	return k.Username + "/" + k.AppName + "/" + k.MachineName
}

type CreatedAPIKey struct {
	APIKey
	Key string `json:"key"`
}

type Token struct {
	ID                   string
	Label                string
	AccountID            string
	AccessToken          string
	RefreshToken         string
	AccessTokenExpiresAt time.Time
}

type TokenSummary struct {
	ID                   string     `json:"id"`
	Label                string     `json:"label"`
	AccountID            string     `json:"account_id"`
	AccessTokenExpiresAt time.Time  `json:"access_token_expires_at"`
	Healthy              bool       `json:"healthy"`
	ConsecutiveFailures  int        `json:"consecutive_failures"`
	ErrorMessage         string     `json:"error_message,omitempty"`
	Enabled              bool       `json:"enabled"`
	CreatedAt            time.Time  `json:"created_at"`
	LastUsedAt           *time.Time `json:"last_used_at"`
	LastRefreshedAt      time.Time  `json:"last_refreshed_at"`
}

type RequestRecord struct {
	APIKeyID                  string
	TokenID                   string
	Model                     string
	StatusCode                int
	Duration                  time.Duration
	Streamed                  bool
	ErrorMessage              string
	ResponseID                string
	UpstreamRequestID         string
	ResponseModel             string
	ResponseStatus            string
	ServiceTier               string
	InputTokens               int64
	CachedTokens              int64
	CacheWriteTokens          int64
	OutputTokens              int64
	ReasoningTokens           int64
	TotalTokens               int64
	WebSearchRequests         int64
	ImageGenInputTokens       int64
	ImageGenInputImageTokens  int64
	ImageGenInputTextTokens   int64
	ImageGenOutputTokens      int64
	ImageGenOutputImageTokens int64
	ImageGenOutputTextTokens  int64
	ImageGenTotalTokens       int64
	ResponseError             string
	IncompleteDetails         string
	CodexHeaders              string
}

type RecentRequest struct {
	ID                   int64     `json:"id"`
	Client               string    `json:"client"`
	Token                string    `json:"token"`
	Model                string    `json:"model"`
	StatusCode           int       `json:"status_code"`
	DurationMS           int64     `json:"duration_ms"`
	Streamed             bool      `json:"streamed"`
	ErrorMessage         string    `json:"error_message,omitempty"`
	ResponseID           string    `json:"response_id,omitempty"`
	UpstreamRequestID    string    `json:"upstream_request_id,omitempty"`
	ResponseModel        string    `json:"response_model,omitempty"`
	ResponseStatus       string    `json:"response_status,omitempty"`
	ServiceTier          string    `json:"service_tier,omitempty"`
	InputTokens          int64     `json:"input_tokens"`
	CachedTokens         int64     `json:"cached_tokens"`
	CacheWriteTokens     int64     `json:"cache_write_tokens"`
	OutputTokens         int64     `json:"output_tokens"`
	ReasoningTokens      int64     `json:"reasoning_tokens"`
	TotalTokens          int64     `json:"total_tokens"`
	WebSearchRequests    int64     `json:"web_search_requests"`
	ImageGenInputTokens  int64     `json:"image_gen_input_tokens"`
	ImageGenOutputTokens int64     `json:"image_gen_output_tokens"`
	ImageGenTotalTokens  int64     `json:"image_gen_total_tokens"`
	CreatedAt            time.Time `json:"created_at"`
}

type Stats struct {
	TotalRequests        int64 `json:"total_requests"`
	TodayRequests        int64 `json:"today_requests"`
	ActiveAPIKeys        int64 `json:"active_api_keys"`
	HealthyTokens        int64 `json:"healthy_tokens"`
	UnhealthyTokens      int64 `json:"unhealthy_tokens"`
	InputTokens          int64 `json:"input_tokens"`
	CachedTokens         int64 `json:"cached_tokens"`
	CacheWriteTokens     int64 `json:"cache_write_tokens"`
	OutputTokens         int64 `json:"output_tokens"`
	ReasoningTokens      int64 `json:"reasoning_tokens"`
	TotalTokens          int64 `json:"total_tokens"`
	WebSearchRequests    int64 `json:"web_search_requests"`
	ImageGenInputTokens  int64 `json:"image_gen_input_tokens"`
	ImageGenOutputTokens int64 `json:"image_gen_output_tokens"`
	ImageGenTotalTokens  int64 `json:"image_gen_total_tokens"`
}

func (s *Store) CreateAPIKey(ctx context.Context, username, appName, machineName string, rateLimitRPS float64) (CreatedAPIKey, error) {
	username = strings.TrimSpace(username)
	appName = strings.TrimSpace(appName)
	machineName = strings.TrimSpace(machineName)
	if username == "" || appName == "" || machineName == "" {
		return CreatedAPIKey{}, errors.New("username, app_name, and machine_name are required")
	}
	if rateLimitRPS <= 0 {
		return CreatedAPIKey{}, errors.New("rate limit must be positive")
	}
	random := make([]byte, 32)
	if _, err := rand.Read(random); err != nil {
		return CreatedAPIKey{}, fmt.Errorf("generate API key: %w", err)
	}
	raw := "cp_" + base64.RawURLEncoding.EncodeToString(random)
	prefix := raw
	if len(prefix) > 12 {
		prefix = prefix[:12]
	}
	hash := hashAPIKey(raw)

	created := CreatedAPIKey{Key: raw}
	err := s.pool.QueryRow(ctx, `
		INSERT INTO api_keys(username, app_name, machine_name, key_hash, key_prefix, rate_limit_rps)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING id::text, username, app_name, machine_name, key_prefix, rate_limit_rps,
			enabled, created_at, last_used_at, total_requests
	`, username, appName, machineName, hash, prefix, rateLimitRPS).Scan(
		&created.ID, &created.Username, &created.AppName, &created.MachineName,
		&created.KeyPrefix, &created.RateLimitRPS, &created.Enabled,
		&created.CreatedAt, &created.LastUsedAt, &created.TotalRequests,
	)
	if err != nil {
		return CreatedAPIKey{}, fmt.Errorf("insert API key: %w", err)
	}
	return created, nil
}

func (s *Store) AuthenticateAPIKey(ctx context.Context, raw string) (APIKey, error) {
	var key APIKey
	err := s.pool.QueryRow(ctx, `
		SELECT id::text, username, app_name, machine_name, key_prefix, rate_limit_rps,
			enabled, created_at, last_used_at, total_requests
		FROM api_keys
		WHERE key_hash = $1 AND enabled = true
	`, hashAPIKey(strings.TrimSpace(raw))).Scan(
		&key.ID, &key.Username, &key.AppName, &key.MachineName,
		&key.KeyPrefix, &key.RateLimitRPS, &key.Enabled,
		&key.CreatedAt, &key.LastUsedAt, &key.TotalRequests,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return APIKey{}, ErrInvalidAPIKey
	}
	if err != nil {
		return APIKey{}, fmt.Errorf("authenticate API key: %w", err)
	}
	return key, nil
}

func (s *Store) ListAPIKeys(ctx context.Context) ([]APIKey, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id::text, username, app_name, machine_name, key_prefix, rate_limit_rps,
			enabled, created_at, last_used_at, total_requests
		FROM api_keys ORDER BY created_at DESC
	`)
	if err != nil {
		return nil, fmt.Errorf("list API keys: %w", err)
	}
	defer rows.Close()

	keys := make([]APIKey, 0)
	for rows.Next() {
		var key APIKey
		if err := rows.Scan(
			&key.ID, &key.Username, &key.AppName, &key.MachineName, &key.KeyPrefix,
			&key.RateLimitRPS, &key.Enabled, &key.CreatedAt, &key.LastUsedAt, &key.TotalRequests,
		); err != nil {
			return nil, fmt.Errorf("scan API key: %w", err)
		}
		keys = append(keys, key)
	}
	return keys, rows.Err()
}

func (s *Store) RevokeAPIKey(ctx context.Context, id string) error {
	result, err := s.pool.Exec(ctx, `UPDATE api_keys SET enabled = false WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("revoke API key: %w", err)
	}
	if result.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) UpsertToken(ctx context.Context, token Token) (TokenSummary, error) {
	accessCiphertext, err := s.box.Encrypt(token.AccessToken)
	if err != nil {
		return TokenSummary{}, fmt.Errorf("encrypt access token: %w", err)
	}
	refreshCiphertext, err := s.box.Encrypt(token.RefreshToken)
	if err != nil {
		return TokenSummary{}, fmt.Errorf("encrypt refresh token: %w", err)
	}

	var summary TokenSummary
	err = s.pool.QueryRow(ctx, `
		INSERT INTO tokens(label, account_id, access_token_ciphertext, refresh_token_ciphertext, access_token_expires_at)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (label) DO UPDATE SET
			account_id = EXCLUDED.account_id,
			access_token_ciphertext = EXCLUDED.access_token_ciphertext,
			refresh_token_ciphertext = EXCLUDED.refresh_token_ciphertext,
			access_token_expires_at = EXCLUDED.access_token_expires_at,
			healthy = true,
			consecutive_failures = 0,
			error_message = NULL,
			enabled = true,
			last_refreshed_at = now()
		RETURNING id::text, label, account_id, access_token_expires_at, healthy,
			consecutive_failures, COALESCE(error_message, ''), enabled, created_at, last_used_at, last_refreshed_at
	`, token.Label, token.AccountID, accessCiphertext, refreshCiphertext, token.AccessTokenExpiresAt).Scan(
		&summary.ID, &summary.Label, &summary.AccountID, &summary.AccessTokenExpiresAt, &summary.Healthy,
		&summary.ConsecutiveFailures, &summary.ErrorMessage, &summary.Enabled, &summary.CreatedAt,
		&summary.LastUsedAt, &summary.LastRefreshedAt,
	)
	if err != nil {
		return TokenSummary{}, fmt.Errorf("upsert token: %w", err)
	}
	return summary, nil
}

func (s *Store) SelectToken(ctx context.Context) (Token, error) {
	return s.queryToken(ctx, `
		WITH selected AS (
			SELECT id FROM tokens
			WHERE enabled = true AND healthy = true
			ORDER BY last_used_at ASC NULLS FIRST, created_at ASC
			FOR UPDATE SKIP LOCKED
			LIMIT 1
		)
		UPDATE tokens SET last_used_at = now()
		WHERE id = (SELECT id FROM selected)
		RETURNING id::text, label, account_id, access_token_ciphertext,
			refresh_token_ciphertext, access_token_expires_at
	`)
}

func (s *Store) GetToken(ctx context.Context, id string) (Token, error) {
	return s.queryToken(ctx, `
		SELECT id::text, label, account_id, access_token_ciphertext,
			refresh_token_ciphertext, access_token_expires_at
		FROM tokens WHERE id = $1 AND enabled = true AND healthy = true
	`, id)
}

func (s *Store) queryToken(ctx context.Context, query string, args ...any) (Token, error) {
	var token Token
	var accessCiphertext, refreshCiphertext string
	err := s.pool.QueryRow(ctx, query, args...).Scan(
		&token.ID, &token.Label, &token.AccountID, &accessCiphertext,
		&refreshCiphertext, &token.AccessTokenExpiresAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return Token{}, ErrNoHealthyTokens
	}
	if err != nil {
		return Token{}, fmt.Errorf("select token: %w", err)
	}
	token.AccessToken, err = s.box.Decrypt(accessCiphertext)
	if err != nil {
		return Token{}, fmt.Errorf("decrypt access token for %s: %w", token.ID, err)
	}
	token.RefreshToken, err = s.box.Decrypt(refreshCiphertext)
	if err != nil {
		return Token{}, fmt.Errorf("decrypt refresh token for %s: %w", token.ID, err)
	}
	return token, nil
}

func (s *Store) UpdateTokenCredentials(ctx context.Context, token Token) error {
	accessCiphertext, err := s.box.Encrypt(token.AccessToken)
	if err != nil {
		return fmt.Errorf("encrypt access token: %w", err)
	}
	refreshCiphertext, err := s.box.Encrypt(token.RefreshToken)
	if err != nil {
		return fmt.Errorf("encrypt refresh token: %w", err)
	}
	result, err := s.pool.Exec(ctx, `
		UPDATE tokens SET account_id = $2, access_token_ciphertext = $3,
			refresh_token_ciphertext = $4, access_token_expires_at = $5,
			healthy = true, consecutive_failures = 0, error_message = NULL,
			last_refreshed_at = now()
		WHERE id = $1
	`, token.ID, token.AccountID, accessCiphertext, refreshCiphertext, token.AccessTokenExpiresAt)
	if err != nil {
		return fmt.Errorf("update token credentials: %w", err)
	}
	if result.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) RecordTokenFailure(ctx context.Context, id, message string, threshold int) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE tokens SET
			consecutive_failures = consecutive_failures + 1,
			error_message = $2,
			healthy = CASE WHEN consecutive_failures + 1 >= $3 THEN false ELSE healthy END
		WHERE id = $1
	`, id, message, threshold)
	if err != nil {
		return fmt.Errorf("record token failure: %w", err)
	}
	return nil
}

func (s *Store) MarkTokenUnhealthy(ctx context.Context, id, message string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE tokens SET healthy = false, consecutive_failures = consecutive_failures + 1, error_message = $2
		WHERE id = $1
	`, id, message)
	if err != nil {
		return fmt.Errorf("mark token unhealthy: %w", err)
	}
	return nil
}

func (s *Store) ListTokens(ctx context.Context) ([]TokenSummary, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id::text, label, account_id, access_token_expires_at, healthy,
			consecutive_failures, COALESCE(error_message, ''), enabled, created_at,
			last_used_at, last_refreshed_at
		FROM tokens ORDER BY created_at DESC
	`)
	if err != nil {
		return nil, fmt.Errorf("list tokens: %w", err)
	}
	defer rows.Close()

	tokens := make([]TokenSummary, 0)
	for rows.Next() {
		var token TokenSummary
		if err := rows.Scan(
			&token.ID, &token.Label, &token.AccountID, &token.AccessTokenExpiresAt,
			&token.Healthy, &token.ConsecutiveFailures, &token.ErrorMessage, &token.Enabled,
			&token.CreatedAt, &token.LastUsedAt, &token.LastRefreshedAt,
		); err != nil {
			return nil, fmt.Errorf("scan token: %w", err)
		}
		tokens = append(tokens, token)
	}
	return tokens, rows.Err()
}

func (s *Store) DisableToken(ctx context.Context, id string) error {
	result, err := s.pool.Exec(ctx, `UPDATE tokens SET enabled = false WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("disable token: %w", err)
	}
	if result.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) HealthyTokenCount(ctx context.Context) (int64, error) {
	var count int64
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM tokens WHERE enabled = true AND healthy = true`).Scan(&count); err != nil {
		return 0, fmt.Errorf("count healthy tokens: %w", err)
	}
	return count, nil
}

func (s *Store) RecordRequest(ctx context.Context, record RequestRecord) error {
	var apiKeyID, tokenID any
	if record.APIKeyID != "" {
		apiKeyID = record.APIKeyID
	}
	if record.TokenID != "" {
		tokenID = record.TokenID
	}
	_, err := s.pool.Exec(ctx, `
		WITH inserted AS (
			INSERT INTO requests(
				api_key_id, token_id, model, status_code, duration_ms, streamed, error_message,
				response_id, upstream_request_id, response_model, response_status, service_tier,
				input_tokens, cached_tokens, cache_write_tokens, output_tokens, reasoning_tokens, total_tokens,
				web_search_requests, image_gen_input_tokens, image_gen_input_image_tokens,
				image_gen_input_text_tokens, image_gen_output_tokens, image_gen_output_image_tokens,
				image_gen_output_text_tokens, image_gen_total_tokens,
				response_error, incomplete_details, codex_headers
			)
			VALUES (
				$1, $2, NULLIF($3, ''), $4, $5, $6, NULLIF($7, ''),
				NULLIF($8, ''), NULLIF($9, ''), NULLIF($10, ''), NULLIF($11, ''), NULLIF($12, ''),
				$13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23, $24, $25, $26,
				NULLIF($27, '')::jsonb, NULLIF($28, '')::jsonb, NULLIF($29, '')::jsonb
			)
		)
		UPDATE api_keys SET last_used_at = now(), total_requests = total_requests + 1 WHERE id = $1
	`, apiKeyID, tokenID, record.Model, record.StatusCode, record.Duration.Milliseconds(), record.Streamed,
		record.ErrorMessage, record.ResponseID, record.UpstreamRequestID, record.ResponseModel,
		record.ResponseStatus, record.ServiceTier, record.InputTokens, record.CachedTokens,
		record.CacheWriteTokens, record.OutputTokens, record.ReasoningTokens, record.TotalTokens,
		record.WebSearchRequests, record.ImageGenInputTokens, record.ImageGenInputImageTokens,
		record.ImageGenInputTextTokens, record.ImageGenOutputTokens, record.ImageGenOutputImageTokens,
		record.ImageGenOutputTextTokens, record.ImageGenTotalTokens, record.ResponseError,
		record.IncompleteDetails, record.CodexHeaders)
	if err != nil {
		return fmt.Errorf("record request: %w", err)
	}
	return nil
}

func (s *Store) RecentRequests(ctx context.Context, limit int) ([]RecentRequest, error) {
	if limit < 1 || limit > 500 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx, `
		SELECT r.id, COALESCE(k.username || '/' || k.app_name || '/' || k.machine_name, ''),
			COALESCE(t.label, ''), COALESCE(r.model, ''),
			r.status_code, r.duration_ms, r.streamed, COALESCE(r.error_message, ''),
			COALESCE(r.response_id, ''), COALESCE(r.upstream_request_id, ''),
			COALESCE(r.response_model, ''), COALESCE(r.response_status, ''), COALESCE(r.service_tier, ''),
			r.input_tokens, r.cached_tokens, r.cache_write_tokens, r.output_tokens,
			r.reasoning_tokens, r.total_tokens, r.web_search_requests,
			r.image_gen_input_tokens, r.image_gen_output_tokens, r.image_gen_total_tokens, r.created_at
		FROM requests r
		LEFT JOIN api_keys k ON k.id = r.api_key_id
		LEFT JOIN tokens t ON t.id = r.token_id
		ORDER BY r.id DESC LIMIT $1
	`, limit)
	if err != nil {
		return nil, fmt.Errorf("list recent requests: %w", err)
	}
	defer rows.Close()

	requests := make([]RecentRequest, 0)
	for rows.Next() {
		var request RecentRequest
		if err := rows.Scan(
			&request.ID, &request.Client, &request.Token, &request.Model, &request.StatusCode,
			&request.DurationMS, &request.Streamed, &request.ErrorMessage, &request.ResponseID,
			&request.UpstreamRequestID, &request.ResponseModel, &request.ResponseStatus,
			&request.ServiceTier, &request.InputTokens, &request.CachedTokens,
			&request.CacheWriteTokens, &request.OutputTokens, &request.ReasoningTokens,
			&request.TotalTokens, &request.WebSearchRequests, &request.ImageGenInputTokens,
			&request.ImageGenOutputTokens, &request.ImageGenTotalTokens, &request.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan recent request: %w", err)
		}
		requests = append(requests, request)
	}
	return requests, rows.Err()
}

func (s *Store) Stats(ctx context.Context) (Stats, error) {
	var stats Stats
	err := s.pool.QueryRow(ctx, `
		SELECT
			(SELECT count(*) FROM requests),
			(SELECT count(*) FROM requests WHERE created_at >= date_trunc('day', now())),
			(SELECT count(*) FROM api_keys WHERE enabled = true),
			(SELECT count(*) FROM tokens WHERE enabled = true AND healthy = true),
			(SELECT count(*) FROM tokens WHERE enabled = true AND healthy = false),
			COALESCE((SELECT sum(input_tokens) FROM requests), 0),
			COALESCE((SELECT sum(cached_tokens) FROM requests), 0),
			COALESCE((SELECT sum(cache_write_tokens) FROM requests), 0),
			COALESCE((SELECT sum(output_tokens) FROM requests), 0),
			COALESCE((SELECT sum(reasoning_tokens) FROM requests), 0),
			COALESCE((SELECT sum(total_tokens) FROM requests), 0),
			COALESCE((SELECT sum(web_search_requests) FROM requests), 0),
			COALESCE((SELECT sum(image_gen_input_tokens) FROM requests), 0),
			COALESCE((SELECT sum(image_gen_output_tokens) FROM requests), 0),
			COALESCE((SELECT sum(image_gen_total_tokens) FROM requests), 0)
	`).Scan(
		&stats.TotalRequests, &stats.TodayRequests, &stats.ActiveAPIKeys,
		&stats.HealthyTokens, &stats.UnhealthyTokens, &stats.InputTokens,
		&stats.CachedTokens, &stats.CacheWriteTokens, &stats.OutputTokens,
		&stats.ReasoningTokens, &stats.TotalTokens, &stats.WebSearchRequests,
		&stats.ImageGenInputTokens, &stats.ImageGenOutputTokens, &stats.ImageGenTotalTokens,
	)
	if err != nil {
		return Stats{}, fmt.Errorf("load stats: %w", err)
	}
	return stats, nil
}

func (s *Store) CleanupRequests(ctx context.Context, olderThan time.Time) (int64, error) {
	result, err := s.pool.Exec(ctx, `DELETE FROM requests WHERE created_at < $1`, olderThan)
	if err != nil {
		return 0, fmt.Errorf("cleanup requests: %w", err)
	}
	return result.RowsAffected(), nil
}

func hashAPIKey(raw string) string {
	hash := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(hash[:])
}
