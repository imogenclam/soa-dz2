package middleware

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
)

type logKey struct{}

// requestLog заполняется по ходу запроса: user_id узнаёт только Auth, который стоит позже.
type requestLog struct {
	userID *int64
}

func setLogUserID(ctx context.Context, id int64) {
	if entry, ok := ctx.Value(logKey{}).(*requestLog); ok {
		entry.userID = &id
	}
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (recorder *statusRecorder) WriteHeader(code int) {
	recorder.status = code
	recorder.ResponseWriter.WriteHeader(code)
}

// Logging пишет одну JSON-строку на каждый запрос и отдаёт X-Request-Id.
func Logging(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			requestID := uuid.NewString()
			w.Header().Set("X-Request-Id", requestID)

			entry := &requestLog{}
			r = r.WithContext(context.WithValue(r.Context(), logKey{}, entry))

			var body any
			mutating := r.Method == http.MethodPost || r.Method == http.MethodPut || r.Method == http.MethodDelete
			if mutating {
				rawBody, _ := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
				r.Body = io.NopCloser(bytes.NewReader(rawBody))
				body = maskBody(rawBody)
			}

			recorder := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(recorder, r)

			var userID any
			if entry.userID != nil {
				userID = *entry.userID
			}
			attrs := []any{
				"request_id", requestID,
				"method", r.Method,
				"endpoint", r.URL.Path,
				"status_code", recorder.status,
				"duration_ms", time.Since(start).Milliseconds(),
				"user_id", userID,
				"timestamp", start.UTC().Format(time.RFC3339Nano),
			}
			if mutating {
				attrs = append(attrs, "request_body", body)
			}
			log.Info("request", attrs...)
		})
	}
}

// maskBody заменяет пароли и токены на "***".
func maskBody(rawBody []byte) any {
	if len(rawBody) == 0 {
		return nil
	}
	var parsed any
	if err := json.Unmarshal(rawBody, &parsed); err != nil {
		return "<not json>"
	}
	return mask(parsed)
}

func mask(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		for key, nested := range typed {
			lowerKey := strings.ToLower(key)
			if strings.Contains(lowerKey, "password") || strings.Contains(lowerKey, "token") {
				typed[key] = "***"
			} else {
				typed[key] = mask(nested)
			}
		}
	case []any:
		for i := range typed {
			typed[i] = mask(typed[i])
		}
	}
	return value
}
