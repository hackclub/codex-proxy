package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/hackclub/codex-proxy/internal/db"
	"github.com/hackclub/codex-proxy/internal/ratelimit"
)

type Handler struct {
	store               *db.Store
	tokens              tokenPool
	limiter             *ratelimit.Limiter
	upstreamURL         string
	defaultInstructions string
	maxRequestBytes     int64
	httpClient          *http.Client
}

type tokenPool interface {
	Borrow(context.Context) (db.Token, error)
	ReportAuthFailure(context.Context, string, string)
	ReportUsage(context.Context, string, http.Header) error
	ReportRateLimit(context.Context, string, http.Header, []byte) (time.Time, error)
}

type HandlerConfig struct {
	UpstreamURL         string
	DefaultInstructions string
	MaxRequestBytes     int64
	HTTPClient          *http.Client
}

func NewHandler(store *db.Store, tokens tokenPool, limiter *ratelimit.Limiter, config HandlerConfig) *Handler {
	client := config.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Minute}
	}
	return &Handler{
		store:               store,
		tokens:              tokens,
		limiter:             limiter,
		upstreamURL:         config.UpstreamURL,
		defaultInstructions: config.DefaultInstructions,
		maxRequestBytes:     config.MaxRequestBytes,
		httpClient:          client,
	}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "only POST is supported")
		return
	}

	rawAPIKey := requestAPIKey(r)
	if rawAPIKey == "" {
		writeError(w, http.StatusUnauthorized, "authentication_error", "missing API key")
		return
	}
	apiKey, err := h.store.AuthenticateAPIKey(r.Context(), rawAPIKey)
	if errors.Is(err, db.ErrInvalidAPIKey) {
		writeError(w, http.StatusUnauthorized, "authentication_error", "invalid API key")
		return
	}
	if err != nil {
		log.Printf("authenticate API key: %v", err)
		writeError(w, http.StatusServiceUnavailable, "proxy_error", "authentication service unavailable")
		return
	}
	if !h.limiter.Allow(apiKey.ID, apiKey.RateLimitRPS) {
		w.Header().Set("Retry-After", "1")
		writeError(w, http.StatusTooManyRequests, "rate_limit_error", "proxy API key rate limit exceeded")
		h.record(started, db.RequestRecord{
			APIKeyID: apiKey.ID, StatusCode: http.StatusTooManyRequests, ErrorMessage: "rate limit exceeded",
		})
		return
	}

	reader := http.MaxBytesReader(w, r.Body, h.maxRequestBytes)
	rawBody, err := io.ReadAll(reader)
	_ = r.Body.Close()
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, "invalid_request_error", "request body is too large")
		} else {
			writeError(w, http.StatusBadRequest, "invalid_request_error", "could not read request body")
		}
		return
	}
	patchedBody, metadata, err := PatchPayload(rawBody, h.defaultInstructions)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	record := db.RequestRecord{APIKeyID: apiKey.ID, Model: metadata.Model, Streamed: metadata.DownstreamStream}

	token, upstreamResponse, err := h.callUpstream(r.Context(), patchedBody, metadata)
	if err != nil {
		var unavailable *db.NoAvailableTokensError
		if errors.As(err, &unavailable) {
			status := http.StatusServiceUnavailable
			typeName := "proxy_error"
			message := "no token accounts are available"
			if unavailable.RetryAt != nil {
				status = http.StatusTooManyRequests
				typeName = "usage_limit_reached"
				message = "all token accounts have reached their usage limits"
				w.Header().Set("Retry-After", retryAfterSeconds(*unavailable.RetryAt))
			}
			writeError(w, status, typeName, message)
			record.StatusCode = status
			record.ErrorMessage = err.Error()
			h.record(started, record)
			return
		}
		log.Printf("upstream request failed: %v", err)
		writeError(w, http.StatusBadGateway, "upstream_error", "Codex upstream request failed")
		record.TokenID = token.ID
		record.StatusCode = http.StatusBadGateway
		record.ErrorMessage = err.Error()
		h.record(started, record)
		return
	}
	defer upstreamResponse.Body.Close()
	record.TokenID = token.ID
	record.UpstreamRequestID = upstreamResponse.Header.Get("X-OAI-Request-ID")
	record.CodexHeaders = codexHeaders(upstreamResponse.Header)

	w.Header().Set("X-Codex-Proxy-Client", apiKey.Label())
	w.Header().Set("X-Codex-Proxy-Token", token.Label)
	copyResponseHeaders(w.Header(), upstreamResponse.Header)

	if upstreamResponse.StatusCode < 200 || upstreamResponse.StatusCode >= 300 {
		responseBody, readErr := io.ReadAll(upstreamResponse.Body)
		applyResponseDetails(&record, responseDetailsFrom(responseBody))
		w.WriteHeader(upstreamResponse.StatusCode)
		_, writeErr := w.Write(responseBody)
		errorMessage := fmt.Sprintf("upstream HTTP %d", upstreamResponse.StatusCode)
		if readErr != nil {
			errorMessage += ": " + readErr.Error()
		} else if writeErr != nil {
			errorMessage += ": " + writeErr.Error()
		}
		record.StatusCode = upstreamResponse.StatusCode
		record.ErrorMessage = errorMessage
		h.record(started, record)
		return
	}

	contentType := strings.ToLower(upstreamResponse.Header.Get("Content-Type"))
	if strings.Contains(contentType, "application/json") {
		responseBody, readErr := io.ReadAll(upstreamResponse.Body)
		applyResponseDetails(&record, responseDetailsFrom(responseBody))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(upstreamResponse.StatusCode)
		_, writeErr := w.Write(responseBody)
		errorMessage := ""
		if readErr != nil {
			errorMessage = readErr.Error()
		} else if writeErr != nil {
			errorMessage = writeErr.Error()
		}
		record.StatusCode = upstreamResponse.StatusCode
		record.ErrorMessage = errorMessage
		h.record(started, record)
		return
	}

	if metadata.DownstreamStream {
		details, err := RelaySSE(w, upstreamResponse.Body, metadata.Model)
		applyResponseDetails(&record, details)
		record.StatusCode = http.StatusOK
		if err != nil {
			record.ErrorMessage = err.Error()
		}
		h.record(started, record)
		return
	}

	responseJSON, details, err := BufferSSE(upstreamResponse.Body, metadata.Model)
	applyResponseDetails(&record, details)
	if err != nil {
		log.Printf("buffer upstream SSE: %v", err)
		writeError(w, http.StatusBadGateway, "upstream_error", "Codex stream could not be converted to a response")
		record.StatusCode = http.StatusBadGateway
		record.ErrorMessage = err.Error()
		h.record(started, record)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(responseJSON)
	record.StatusCode = http.StatusOK
	h.record(started, record)
}

func (h *Handler) callUpstream(ctx context.Context, body []byte, metadata RequestMetadata) (db.Token, *http.Response, error) {
	var lastToken db.Token
	var lastRateLimit *http.Response
	attempted := make(map[string]bool)
	for {
		token, err := h.tokens.Borrow(ctx)
		if err != nil {
			if lastRateLimit != nil && errors.Is(err, db.ErrNoAvailableTokens) {
				return lastToken, lastRateLimit, nil
			}
			return lastToken, nil, err
		}
		if attempted[token.ID] {
			return lastToken, nil, errors.New("token pool selected the same rejected token twice")
		}
		attempted[token.ID] = true
		lastToken = token

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.upstreamURL, bytes.NewReader(body))
		if err != nil {
			return token, nil, fmt.Errorf("create upstream request: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+token.AccessToken)
		req.Header.Set("ChatGPT-Account-ID", token.AccountID)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "text/event-stream")
		req.Header.Set("User-Agent", "codex-proxy/1.0")
		req.Header.Set("OpenAI-Beta", "responses=experimental")
		req.Header.Set("Originator", "codex-proxy")
		req.Header.Set("Version", "0.144.1")
		if metadata.ResponsesLite {
			req.Header.Set("X-OpenAI-Internal-Codex-Responses-Lite", "true")
		}

		response, err := h.httpClient.Do(req)
		if err != nil {
			return token, nil, err
		}
		if response.StatusCode == http.StatusTooManyRequests {
			responseBody, readErr := io.ReadAll(response.Body)
			_ = response.Body.Close()
			if readErr != nil {
				return token, nil, fmt.Errorf("read upstream rate limit: %w", readErr)
			}
			response.Body = io.NopCloser(bytes.NewReader(responseBody))
			limitedUntil, reportErr := h.tokens.ReportRateLimit(ctx, token.ID, response.Header, responseBody)
			if reportErr != nil {
				log.Printf("record token rate limit: %v", reportErr)
				return token, response, nil
			}
			if response.Header.Get("Retry-After") == "" {
				response.Header.Set("Retry-After", retryAfterSeconds(limitedUntil))
			}
			lastRateLimit = response
			continue
		}

		if response.StatusCode != http.StatusUnauthorized && response.StatusCode != http.StatusForbidden {
			if err := h.tokens.ReportUsage(ctx, token.ID, response.Header); err != nil {
				log.Printf("record token usage: %v", err)
			}
			return token, response, nil
		}

		h.tokens.ReportAuthFailure(ctx, token.ID, fmt.Sprintf("upstream HTTP %d", response.StatusCode))
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
	}
}

func retryAfterSeconds(retryAt time.Time) string {
	return strconv.FormatInt(max(1, int64(math.Ceil(time.Until(retryAt).Seconds()))), 10)
}

func (h *Handler) record(started time.Time, record db.RequestRecord) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	record.Duration = time.Since(started)
	if err := h.store.RecordRequest(ctx, record); err != nil {
		log.Printf("record request: %v", err)
	}
}

func applyResponseDetails(record *db.RequestRecord, details responseDetails) {
	record.ResponseID = details.ID
	record.ResponseModel = details.Model
	record.ResponseStatus = details.Status
	record.ServiceTier = details.ServiceTier
	record.InputTokens = details.InputTokens
	record.CachedTokens = details.CachedTokens
	record.CacheWriteTokens = details.CacheWriteTokens
	record.OutputTokens = details.OutputTokens
	record.ReasoningTokens = details.ReasoningTokens
	record.TotalTokens = details.TotalTokens
	record.WebSearchRequests = details.WebSearchRequests
	record.ImageGenInputTokens = details.ImageGen.InputTokens
	record.ImageGenInputImageTokens = details.ImageGen.InputImageTokens
	record.ImageGenInputTextTokens = details.ImageGen.InputTextTokens
	record.ImageGenOutputTokens = details.ImageGen.OutputTokens
	record.ImageGenOutputImageTokens = details.ImageGen.OutputImageTokens
	record.ImageGenOutputTextTokens = details.ImageGen.OutputTextTokens
	record.ImageGenTotalTokens = details.ImageGen.TotalTokens
	record.ResponseError = string(details.Error)
	record.IncompleteDetails = string(details.IncompleteDetails)
}

func codexHeaders(headers http.Header) string {
	values := make(map[string][]string)
	for key, value := range headers {
		if strings.HasPrefix(strings.ToLower(key), "x-codex-") {
			values[key] = value
		}
	}
	if len(values) == 0 {
		return ""
	}
	encoded, _ := json.Marshal(values)
	return string(encoded)
}

func requestAPIKey(r *http.Request) string {
	if key := strings.TrimSpace(r.Header.Get("X-API-Key")); key != "" {
		return key
	}
	authorization := strings.TrimSpace(r.Header.Get("Authorization"))
	scheme, token, found := strings.Cut(authorization, " ")
	if found && strings.EqualFold(scheme, "Bearer") {
		return strings.TrimSpace(token)
	}
	return ""
}

func copyResponseHeaders(destination, source http.Header) {
	for key, values := range source {
		if blockedResponseHeader(key) {
			continue
		}
		for _, value := range values {
			destination.Add(key, value)
		}
	}
}

func blockedResponseHeader(key string) bool {
	switch strings.ToLower(key) {
	case "connection", "content-length", "keep-alive", "proxy-authenticate", "proxy-authorization",
		"set-cookie", "te", "trailer", "transfer-encoding", "upgrade":
		return true
	default:
		return false
	}
}

func writeError(w http.ResponseWriter, status int, errorType, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"type":    errorType,
			"message": message,
		},
	})
}
