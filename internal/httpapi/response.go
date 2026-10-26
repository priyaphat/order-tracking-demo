package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
)

type contextKey string

const requestIDKey contextKey = "request-id"

type ErrorBody struct {
	Code      string            `json:"code"`
	Message   string            `json:"message"`
	Fields    map[string]string `json:"fieldErrors,omitempty"`
	RequestID string            `json:"requestId"`
}

func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey, id)
}

func RequestID(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey).(string)
	return id
}

func JSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func Error(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	JSON(w, status, ErrorBody{Code: code, Message: message, RequestID: RequestID(r.Context())})
}

func ValidationError(w http.ResponseWriter, r *http.Request, fields map[string]string) {
	JSON(w, http.StatusBadRequest, ErrorBody{Code: "validation_failed", Message: "request validation failed", Fields: fields, RequestID: RequestID(r.Context())})
}

func DecodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		Error(w, r, http.StatusBadRequest, "invalid_json", "request body must be valid JSON")
		return false
	}
	return true
}
