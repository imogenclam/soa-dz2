// Package apperr — ошибки API в едином формате из контракта (схема Error).
package apperr

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"marketplace/internal/api"
)

type Error struct {
	Status  int
	Code    api.ErrorCode
	Message string
	Details map[string]any
}

func (apiErr *Error) Error() string { return string(apiErr.Code) + ": " + apiErr.Message }

func New(status int, code api.ErrorCode, message string) *Error {
	return &Error{Status: status, Code: code, Message: message}
}

func (apiErr *Error) WithDetails(details map[string]any) *Error {
	apiErr.Details = details
	return apiErr
}

func Validation(fields []FieldError) *Error {
	return New(http.StatusBadRequest, api.VALIDATIONERROR, "invalid request").
		WithDetails(map[string]any{"fields": fields})
}

type FieldError struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

func AccessDenied() *Error {
	return New(http.StatusForbidden, api.ACCESSDENIED, "not enough rights")
}

// Write отдаёт ошибку клиенту. Всё, что не *Error, превращается в 500 без подробностей.
func Write(w http.ResponseWriter, r *http.Request, err error) {
	var apiErr *Error
	if !errors.As(err, &apiErr) {
		slog.Error("internal error", "request_id", w.Header().Get("X-Request-Id"), "endpoint", r.URL.Path, "error", err.Error())
		apiErr = New(http.StatusInternalServerError, api.INTERNALERROR, "internal error")
	}
	body := api.Error{ErrorCode: apiErr.Code, Message: apiErr.Message}
	if apiErr.Details != nil {
		body.Details = &apiErr.Details
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(apiErr.Status)
	_ = json.NewEncoder(w).Encode(body)
}
