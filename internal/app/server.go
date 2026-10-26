package app

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pkp/order-tracking-demo/internal/config"
	"github.com/pkp/order-tracking-demo/internal/httpapi"
	"github.com/pkp/order-tracking-demo/internal/order"
)

type Server struct {
	cfg    config.Config
	db     *pgxpool.Pool
	store  *order.Store
	logger *slog.Logger
}

func New(cfg config.Config, db *pgxpool.Pool, logger *slog.Logger) *Server {
	return &Server{cfg: cfg, db: db, store: order.NewStore(db, cfg.LeaseDuration, cfg.MaxRetries, cfg.QueueSLA), logger: logger}
}

func (s *Server) Router() http.Handler {
	r := chi.NewRouter()
	r.Use(s.requestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Recoverer)
	r.Use(middleware.Timeout(15 * time.Second))
	r.Use(s.accessLog)
	r.Get("/", func(w http.ResponseWriter, r *http.Request) {
		httpapi.JSON(w, http.StatusOK, map[string]any{"name": "Order Tracking API", "docs": "/openapi.yaml", "health": "/health/ready"})
	})
	r.Get("/health/live", func(w http.ResponseWriter, r *http.Request) { httpapi.JSON(w, 200, map[string]string{"status": "ok"}) })
	r.Get("/health/ready", s.ready)
	r.Get("/openapi.yaml", s.openAPI)

	r.Route("/api/v1/orders", func(r chi.Router) {
		r.Post("/", s.createOrder)
		r.Get("/", s.listOrders)
		r.Get("/{orderNo}", s.getOrder)
		r.Get("/{orderNo}/timeline", s.timeline)
		r.Post("/{orderNo}/report-issue", s.reportIssue)
	})
	r.Route("/api/v1/internal", func(r chi.Router) {
		r.Use(s.requireInternalToken)
		r.Post("/bots/claim", s.claim)
		r.Post("/bots/heartbeat", s.heartbeat)
		r.Post("/orders/{orderNo}/result", s.botResult)
		r.Patch("/orders/{orderNo}/player-uid", s.correctUID)
		r.Post("/bank/webhooks", s.bankWebhook)
		r.Post("/maintenance/sweep", s.sweep)
	})
	return r
}

func (s *Server) ready(w http.ResponseWriter, r *http.Request) {
	if err := s.db.Ping(r.Context()); err != nil {
		httpapi.Error(w, r, http.StatusServiceUnavailable, "database_unavailable", "database is unavailable")
		return
	}
	httpapi.JSON(w, 200, map[string]string{"status": "ready"})
}

func (s *Server) openAPI(w http.ResponseWriter, r *http.Request) {
	http.ServeFile(w, r, "/app/openapi.yaml")
}

func (s *Server) createOrder(w http.ResponseWriter, r *http.Request) {
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if len(key) < 8 || len(key) > 128 {
		httpapi.ValidationError(w, r, map[string]string{"Idempotency-Key": "header must contain 8-128 characters"})
		return
	}
	var input order.CreateInput
	if !httpapi.DecodeJSON(w, r, &input) {
		return
	}
	if fields := input.Validate(); len(fields) > 0 {
		httpapi.ValidationError(w, r, fields)
		return
	}
	o, replayed, err := s.store.Create(r.Context(), key, order.RequestHash(input), input, time.Now().Add(s.cfg.PaymentTTL))
	if errors.Is(err, order.ErrConflict) {
		httpapi.Error(w, r, http.StatusConflict, "idempotency_conflict", "Idempotency-Key was already used with a different request")
		return
	}
	if err != nil {
		s.fail(w, r, "create order", err)
		return
	}
	w.Header().Set("Location", "/api/v1/orders/"+o.OrderNo)
	if replayed {
		w.Header().Set("Idempotent-Replayed", "true")
		httpapi.JSON(w, http.StatusOK, map[string]any{"data": o})
		return
	}
	httpapi.JSON(w, http.StatusCreated, map[string]any{"data": o})
}

func (s *Server) getOrder(w http.ResponseWriter, r *http.Request) {
	orderNo, ok := validOrderNo(w, r)
	if !ok {
		return
	}
	o, err := s.store.Get(r.Context(), orderNo)
	if s.handleStoreError(w, r, err) {
		return
	}
	httpapi.JSON(w, 200, map[string]any{"data": o})
}

func (s *Server) listOrders(w http.ResponseWriter, r *http.Request) {
	tab := r.URL.Query().Get("tab")
	if tab == "" {
		tab = "current"
	}
	if tab != "current" && tab != "history" {
		httpapi.ValidationError(w, r, map[string]string{"tab": "must be current or history"})
		return
	}
	page, ok := positiveInt(r.URL.Query().Get("page"), 1, 1, 1_000_000)
	if !ok {
		httpapi.ValidationError(w, r, map[string]string{"page": "must be a positive integer"})
		return
	}
	pageSize, ok := positiveInt(r.URL.Query().Get("pageSize"), 20, 1, 100)
	if !ok {
		httpapi.ValidationError(w, r, map[string]string{"pageSize": "must be between 1 and 100"})
		return
	}
	result, err := s.store.List(r.Context(), tab, page, pageSize)
	if err != nil {
		s.fail(w, r, "list orders", err)
		return
	}
	pages := int((result.Total + int64(pageSize) - 1) / int64(pageSize))
	httpapi.JSON(w, 200, map[string]any{"data": result.Items, "meta": map[string]any{"page": page, "pageSize": pageSize, "total": result.Total, "totalPages": pages}})
}

func (s *Server) timeline(w http.ResponseWriter, r *http.Request) {
	orderNo, ok := validOrderNo(w, r)
	if !ok {
		return
	}
	events, err := s.store.Timeline(r.Context(), orderNo)
	if s.handleStoreError(w, r, err) {
		return
	}
	httpapi.JSON(w, 200, map[string]any{"data": events})
}

func (s *Server) reportIssue(w http.ResponseWriter, r *http.Request) {
	orderNo, ok := validOrderNo(w, r)
	if !ok {
		return
	}
	var input struct {
		IssueType   string `json:"issueType"`
		Description string `json:"description"`
	}
	if !httpapi.DecodeJSON(w, r, &input) {
		return
	}
	fields := map[string]string{}
	if len(strings.TrimSpace(input.IssueType)) < 2 || len(input.IssueType) > 50 {
		fields["issueType"] = "must contain 2-50 characters"
	}
	if len(strings.TrimSpace(input.Description)) < 5 || len(input.Description) > 1000 {
		fields["description"] = "must contain 5-1000 characters"
	}
	if len(fields) > 0 {
		httpapi.ValidationError(w, r, fields)
		return
	}
	id, err := s.store.ReportIssue(r.Context(), orderNo, input.IssueType, input.Description)
	if s.handleStoreError(w, r, err) {
		return
	}
	httpapi.JSON(w, http.StatusCreated, map[string]any{"data": map[string]any{"issueId": id, "status": "OPEN"}})
}

func (s *Server) claim(w http.ResponseWriter, r *http.Request) {
	var input struct {
		WorkerID string `json:"workerId"`
	}
	if !httpapi.DecodeJSON(w, r, &input) {
		return
	}
	if len(strings.TrimSpace(input.WorkerID)) < 2 || len(input.WorkerID) > 100 {
		httpapi.ValidationError(w, r, map[string]string{"workerId": "must contain 2-100 characters"})
		return
	}
	o, err := s.store.Claim(r.Context(), input.WorkerID)
	if err != nil {
		s.fail(w, r, "claim order", err)
		return
	}
	if o == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	httpapi.JSON(w, 200, map[string]any{"data": o})
}

func (s *Server) heartbeat(w http.ResponseWriter, r *http.Request) {
	var input struct {
		WorkerID string `json:"workerId"`
		OrderNo  string `json:"orderNo"`
	}
	if !httpapi.DecodeJSON(w, r, &input) {
		return
	}
	if !order.ValidOrderNo(input.OrderNo) {
		httpapi.ValidationError(w, r, map[string]string{"orderNo": "invalid order number format"})
		return
	}
	err := s.store.Heartbeat(r.Context(), input.OrderNo, input.WorkerID)
	if errors.Is(err, order.ErrLeaseLost) {
		httpapi.Error(w, r, http.StatusConflict, "lease_lost", "worker does not own an active lease")
		return
	}
	if err != nil {
		s.fail(w, r, "heartbeat", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) botResult(w http.ResponseWriter, r *http.Request) {
	orderNo, ok := validOrderNo(w, r)
	if !ok {
		return
	}
	var input struct {
		WorkerID  string `json:"workerId"`
		Outcome   string `json:"outcome"`
		ErrorCode string `json:"errorCode"`
		Message   string `json:"message"`
	}
	if !httpapi.DecodeJSON(w, r, &input) {
		return
	}
	if input.Outcome != "success" && input.Outcome != "transient_failure" && input.Outcome != "permanent_failure" {
		httpapi.ValidationError(w, r, map[string]string{"outcome": "must be success, transient_failure, or permanent_failure"})
		return
	}
	o, err := s.store.Result(r.Context(), orderNo, input.WorkerID, input.Outcome, input.ErrorCode, input.Message)
	if s.handleStoreError(w, r, err) {
		return
	}
	httpapi.JSON(w, 200, map[string]any{"data": o})
}

func (s *Server) correctUID(w http.ResponseWriter, r *http.Request) {
	orderNo, ok := validOrderNo(w, r)
	if !ok {
		return
	}
	var input struct {
		PlayerUID string `json:"playerUid"`
	}
	if !httpapi.DecodeJSON(w, r, &input) {
		return
	}
	if len(strings.TrimSpace(input.PlayerUID)) < 1 || len(input.PlayerUID) > 100 {
		httpapi.ValidationError(w, r, map[string]string{"playerUid": "must contain 1-100 characters"})
		return
	}
	o, err := s.store.CorrectUIDAndRetry(r.Context(), orderNo, input.PlayerUID)
	if s.handleStoreError(w, r, err) {
		return
	}
	httpapi.JSON(w, 200, map[string]any{"data": o})
}

func (s *Server) bankWebhook(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		httpapi.Error(w, r, 400, "invalid_body", "request body is invalid")
		return
	}
	if !validSignature(raw, r.Header.Get("X-Webhook-Signature"), s.cfg.BankWebhookSecret) {
		httpapi.Error(w, r, http.StatusUnauthorized, "invalid_signature", "webhook signature is invalid")
		return
	}
	var input struct {
		EventID string `json:"eventId"`
		OrderNo string `json:"orderNo"`
		Status  string `json:"status"`
	}
	if err = json.Unmarshal(raw, &input); err != nil {
		httpapi.Error(w, r, 400, "invalid_json", "request body must be valid JSON")
		return
	}
	if input.EventID == "" || !order.ValidOrderNo(input.OrderNo) || (input.Status != "paid" && input.Status != "refunded") {
		httpapi.Error(w, r, 400, "validation_failed", "eventId, orderNo, or status is invalid")
		return
	}
	result, duplicate, err := s.store.BankWebhook(r.Context(), input.EventID, input.OrderNo, input.Status, raw)
	if s.handleStoreError(w, r, err) {
		return
	}
	httpapi.JSON(w, 200, map[string]any{"data": map[string]any{"result": result, "duplicate": duplicate}})
}

func (s *Server) sweep(w http.ResponseWriter, r *http.Request) {
	stats, err := s.store.Sweep(r.Context())
	if err != nil {
		s.fail(w, r, "sweep", err)
		return
	}
	httpapi.JSON(w, 200, map[string]any{"data": stats})
}

func validSignature(body []byte, signature, secret string) bool {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(body)
	expected := hex.EncodeToString(mac.Sum(nil))
	return subtle.ConstantTimeCompare([]byte(strings.ToLower(signature)), []byte(expected)) == 1
}

func validOrderNo(w http.ResponseWriter, r *http.Request) (string, bool) {
	value := chi.URLParam(r, "orderNo")
	if !order.ValidOrderNo(value) {
		httpapi.Error(w, r, http.StatusBadRequest, "invalid_order_no", "order number must match ORD-<13 digits>-<3 digits>")
		return "", false
	}
	return value, true
}

func positiveInt(raw string, fallback, min, max int) (int, bool) {
	if raw == "" {
		return fallback, true
	}
	value, err := strconv.Atoi(raw)
	return value, err == nil && value >= min && value <= max
}

func (s *Server) handleStoreError(w http.ResponseWriter, r *http.Request, err error) bool {
	if err == nil {
		return false
	}
	switch {
	case errors.Is(err, order.ErrNotFound):
		httpapi.Error(w, r, http.StatusNotFound, "order_not_found", "order was not found")
	case errors.Is(err, order.ErrLeaseLost):
		httpapi.Error(w, r, http.StatusConflict, "lease_lost", "worker does not own an active lease")
	case errors.Is(err, order.ErrInvalidTransition):
		httpapi.Error(w, r, http.StatusConflict, "invalid_state_transition", "order cannot transition from its current status")
	default:
		s.fail(w, r, "store operation", err)
	}
	return true
}

func (s *Server) fail(w http.ResponseWriter, r *http.Request, operation string, err error) {
	s.logger.Error(operation, "error", err, "request_id", httpapi.RequestID(r.Context()), "order_no", chi.URLParam(r, "orderNo"))
	httpapi.Error(w, r, http.StatusInternalServerError, "internal_error", "an internal error occurred")
}

func (s *Server) requireInternalToken(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		provided := r.Header.Get("X-Internal-Token")
		if len(provided) != len(s.cfg.InternalAPIToken) || subtle.ConstantTimeCompare([]byte(provided), []byte(s.cfg.InternalAPIToken)) != 1 {
			httpapi.Error(w, r, http.StatusUnauthorized, "invalid_internal_token", "internal token is invalid")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) requestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimSpace(r.Header.Get("X-Request-ID"))
		if id == "" || len(id) > 128 {
			id = uuid.NewString()
		}
		w.Header().Set("X-Request-ID", id)
		next.ServeHTTP(w, r.WithContext(httpapi.WithRequestID(r.Context(), id)))
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (s *Server) accessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: 200}
		next.ServeHTTP(sw, r)
		s.logger.Info("request", "method", r.Method, "path", r.URL.Path, "status", sw.status, "duration_ms", time.Since(started).Milliseconds(), "request_id", httpapi.RequestID(r.Context()), "order_no", chi.URLParam(r, "orderNo"))
	})
}

func (s *Server) RunSweeper(ctxDone <-chan struct{}) {
	ticker := time.NewTicker(s.cfg.SweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctxDone:
			return
		case <-ticker.C:
			ctx, cancel := contextWithTimeout(10 * time.Second)
			stats, err := s.store.Sweep(ctx)
			cancel()
			if err != nil {
				s.logger.Error("background sweep", "error", err)
			} else if stats.Expired+stats.Retried+stats.Delayed+stats.Alerts > 0 {
				s.logger.Info("background sweep", "expired", stats.Expired, "retried", stats.Retried, "delayed", stats.Delayed, "alerts", stats.Alerts)
			}
		}
	}
}

var contextWithTimeout = func(timeout time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), timeout)
}
