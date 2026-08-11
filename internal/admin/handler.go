package admin

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/hackclub/codex-proxy/internal/db"
	"github.com/hackclub/codex-proxy/internal/token"
)

type Handler struct {
	store     *db.Store
	tokens    *token.Manager
	adminUser string
	adminPass string
	template  *template.Template
}

type pageData struct {
	Stats      db.Stats
	APIKeys    []db.APIKey
	Tokens     []db.TokenSummary
	Requests   []db.RecentRequest
	CSRFToken  string
	CreatedKey string
	Notice     string
}

type dashboardSnapshot struct {
	Stats    db.Stats                   `json:"stats"`
	APIKeys  []db.APIKey                `json:"api_keys"`
	KeyUsage map[string][]db.DailyUsage `json:"key_usage"`
	Tokens   []db.TokenSummary          `json:"tokens"`
	Requests []db.RecentRequest         `json:"requests"`
}

func NewHandler(store *db.Store, tokens *token.Manager, adminUser, adminPass string) *Handler {
	return &Handler{
		store:     store,
		tokens:    tokens,
		adminUser: adminUser,
		adminPass: adminPass,
		template:  template.Must(template.New("admin.html").Funcs(template.FuncMap{"timeago": timeAgo}).ParseFS(templates, "templates/admin.html")),
	}
}

func (h *Handler) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /admin", h.dashboard)
	mux.HandleFunc("GET /admin/", h.dashboard)
	mux.HandleFunc("GET /admin/dashboard.js", h.dashboardScript)
	mux.HandleFunc("GET /admin/data", h.dashboardData)
	mux.HandleFunc("POST /admin/keys", h.createKey)
	mux.HandleFunc("DELETE /admin/keys/{id}", h.revokeKey)
	mux.HandleFunc("POST /admin/keys/{id}/revoke", h.revokeKey)
	mux.HandleFunc("POST /admin/tokens", h.addToken)
	mux.HandleFunc("DELETE /admin/tokens/{id}", h.disableToken)
	mux.HandleFunc("POST /admin/tokens/{id}/remove", h.disableToken)
	mux.HandleFunc("GET /admin/stats", h.stats)
	return h.securityHeaders(h.basicAuth(mux))
}

func (h *Handler) dashboard(w http.ResponseWriter, r *http.Request) {
	data, err := h.loadPageData(r.Context())
	if err != nil {
		respondError(w, http.StatusInternalServerError, err)
		return
	}
	data.CSRFToken = issueCSRFToken(w, r)
	data.Notice = r.URL.Query().Get("notice")
	h.render(w, http.StatusOK, data)
}

func (h *Handler) dashboardScript(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	_, _ = w.Write(dashboardJS)
}

func (h *Handler) dashboardData(w http.ResponseWriter, r *http.Request) {
	data, err := h.loadPageData(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	usage, err := h.store.APIKeyUsage(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, dashboardSnapshot{
		Stats: data.Stats, APIKeys: data.APIKeys, KeyUsage: usage,
		Tokens: data.Tokens, Requests: data.Requests,
	})
}

func (h *Handler) createKey(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Username     string   `json:"username"`
		AppName      string   `json:"app_name"`
		MachineName  string   `json:"machine_name"`
		RateLimitRPS *float64 `json:"rate_limit_rps"`
	}
	jsonRequest := isJSON(r)
	if jsonRequest {
		if err := decodeJSON(r, &input); err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
	} else {
		if !validCSRF(r) {
			respondError(w, http.StatusForbidden, errors.New("invalid CSRF token"))
			return
		}
		input.Username = r.FormValue("username")
		input.AppName = r.FormValue("app_name")
		input.MachineName = r.FormValue("machine_name")
		if value := strings.TrimSpace(r.FormValue("rate_limit_rps")); value != "" {
			rate, err := strconv.ParseFloat(value, 64)
			if err != nil {
				respondError(w, http.StatusBadRequest, errors.New("rate_limit_rps must be a number"))
				return
			}
			input.RateLimitRPS = &rate
		}
	}
	rateLimitRPS := 10.0
	if input.RateLimitRPS != nil {
		rateLimitRPS = *input.RateLimitRPS
	}
	created, err := h.store.CreateAPIKey(r.Context(), input.Username, input.AppName, input.MachineName, rateLimitRPS)
	if err != nil {
		if jsonRequest {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		respondError(w, http.StatusBadRequest, err)
		return
	}
	if jsonRequest {
		writeJSON(w, http.StatusCreated, created)
		return
	}
	data, loadErr := h.loadPageData(r.Context())
	if loadErr != nil {
		respondError(w, http.StatusInternalServerError, loadErr)
		return
	}
	data.CSRFToken = issueCSRFToken(w, r)
	data.CreatedKey = created.Key
	data.Notice = "API key created. Copy it now; it will not be shown again."
	h.render(w, http.StatusCreated, data)
}

func (h *Handler) revokeKey(w http.ResponseWriter, r *http.Request) {
	if !authorizeMutation(r) {
		h.mutationError(w, r, http.StatusForbidden, "invalid CSRF token")
		return
	}
	if err := h.store.RevokeAPIKey(r.Context(), r.PathValue("id")); err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, db.ErrNotFound) {
			status = http.StatusNotFound
		}
		h.mutationError(w, r, status, err.Error())
		return
	}
	h.mutationSuccess(w, r, "API key revoked")
}

func (h *Handler) addToken(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Label        string `json:"label"`
		RefreshToken string `json:"refresh_token"`
		AccountID    string `json:"account_id"`
	}
	jsonRequest := isJSON(r)
	if jsonRequest {
		if err := decodeJSON(r, &input); err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
	} else {
		if !validCSRF(r) {
			respondError(w, http.StatusForbidden, errors.New("invalid CSRF token"))
			return
		}
		input.Label = r.FormValue("label")
		input.RefreshToken = r.FormValue("refresh_token")
		input.AccountID = r.FormValue("account_id")
	}

	donated, err := h.tokens.Donate(r.Context(), input.Label, input.RefreshToken, input.AccountID)
	if err != nil {
		status := http.StatusBadGateway
		if errors.Is(err, token.ErrInvalidDonation) {
			status = http.StatusBadRequest
		}
		if jsonRequest {
			writeJSONError(w, status, err.Error())
			return
		}
		respondError(w, status, err)
		return
	}
	if jsonRequest {
		writeJSON(w, http.StatusCreated, donated)
		return
	}
	h.redirectNotice(w, r, "Token refreshed, encrypted, and added to the pool")
}

func (h *Handler) disableToken(w http.ResponseWriter, r *http.Request) {
	if !authorizeMutation(r) {
		h.mutationError(w, r, http.StatusForbidden, "invalid CSRF token")
		return
	}
	if err := h.store.DisableToken(r.Context(), r.PathValue("id")); err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, db.ErrNotFound) {
			status = http.StatusNotFound
		}
		h.mutationError(w, r, status, err.Error())
		return
	}
	h.mutationSuccess(w, r, "Token removed from rotation")
}

func (h *Handler) stats(w http.ResponseWriter, r *http.Request) {
	stats, err := h.store.Stats(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, stats)
}

func (h *Handler) loadPageData(ctx context.Context) (pageData, error) {
	stats, err := h.store.Stats(ctx)
	if err != nil {
		return pageData{}, err
	}
	keys, err := h.store.ListAPIKeys(ctx)
	if err != nil {
		return pageData{}, err
	}
	tokens, err := h.store.ListTokens(ctx)
	if err != nil {
		return pageData{}, err
	}
	requests, err := h.store.RecentRequests(ctx, 100)
	if err != nil {
		return pageData{}, err
	}
	return pageData{Stats: stats, APIKeys: keys, Tokens: tokens, Requests: requests}, nil
}

func (h *Handler) basicAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || subtle.ConstantTimeCompare([]byte(user), []byte(h.adminUser)) != 1 || subtle.ConstantTimeCompare([]byte(pass), []byte(h.adminPass)) != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="codex-proxy admin", charset="UTF-8"`)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (h *Handler) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; script-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		next.ServeHTTP(w, r)
	})
}

func (h *Handler) render(w http.ResponseWriter, status int, data pageData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := h.template.ExecuteTemplate(w, "admin.html", data); err != nil {
		fmt.Printf("render admin template: %v\n", err)
	}
}

func (h *Handler) mutationSuccess(w http.ResponseWriter, r *http.Request, notice string) {
	if r.Method == http.MethodDelete || isJSON(r) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
		return
	}
	h.redirectNotice(w, r, notice)
}

func (h *Handler) mutationError(w http.ResponseWriter, r *http.Request, status int, message string) {
	if r.Method == http.MethodDelete || isJSON(r) {
		writeJSONError(w, status, message)
		return
	}
	respondError(w, status, errors.New(message))
}

func (h *Handler) redirectNotice(w http.ResponseWriter, r *http.Request, notice string) {
	http.Redirect(w, r, "/admin?notice="+url.QueryEscape(notice), http.StatusSeeOther)
}

func isJSON(r *http.Request) bool {
	return strings.HasPrefix(strings.ToLower(r.Header.Get("Content-Type")), "application/json")
}

func decodeJSON(r *http.Request, destination any) error {
	body, err := io.ReadAll(io.LimitReader(r.Body, (1<<20)+1))
	if err != nil {
		return err
	}
	if len(body) > 1<<20 {
		return errors.New("request body is too large")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return fmt.Errorf("invalid JSON: %w", err)
	}
	return nil
}

func respondError(w http.ResponseWriter, status int, err error) {
	http.Error(w, err.Error(), status)
}

func authorizeMutation(r *http.Request) bool {
	if r.Method == http.MethodDelete && r.Header.Get("Origin") == "" {
		return true
	}
	return validCSRF(r)
}

func issueCSRFToken(w http.ResponseWriter, r *http.Request) string {
	if cookie, err := r.Cookie("codex_proxy_csrf"); err == nil && len(cookie.Value) >= 32 {
		return cookie.Value
	}
	random := make([]byte, 32)
	_, _ = rand.Read(random)
	token := base64.RawURLEncoding.EncodeToString(random)
	http.SetCookie(w, &http.Cookie{
		Name:     "codex_proxy_csrf",
		Value:    token,
		Path:     "/admin",
		HttpOnly: true,
		Secure:   r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https",
		SameSite: http.SameSiteStrictMode,
		MaxAge:   7 * 24 * 60 * 60,
	})
	return token
}

func validCSRF(r *http.Request) bool {
	cookie, err := r.Cookie("codex_proxy_csrf")
	if err != nil {
		return false
	}
	provided := r.Header.Get("X-CSRF-Token")
	if provided == "" {
		provided = r.FormValue("csrf_token")
	}
	return subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(provided)) == 1
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeJSONError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"message": message}})
}

func timeAgo(value any) string {
	var t time.Time
	switch value := value.(type) {
	case time.Time:
		t = value
	case *time.Time:
		if value == nil {
			return "never"
		}
		t = *value
	default:
		return "never"
	}
	duration := time.Since(t)
	switch {
	case duration < time.Minute:
		return "just now"
	case duration < time.Hour:
		return fmt.Sprintf("%dm ago", int(duration.Minutes()))
	case duration < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(duration.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(duration.Hours()/24))
	}
}
