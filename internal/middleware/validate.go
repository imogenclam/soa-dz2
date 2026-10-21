package middleware

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	nethttpmw "github.com/oapi-codegen/nethttp-middleware"

	"marketplace/internal/api"
	"marketplace/internal/apperr"
)

// Validate проверяет запрос по openapi.yaml до того, как он попадёт в хэндлер.
// Все ограничения (длины, minimum, pattern, minItems) берутся только из спецификации.
func Validate(spec *openapi3.T) func(http.Handler) http.Handler {
	return nethttpmw.OapiRequestValidatorWithOptions(spec, &nethttpmw.Options{
		DoNotValidateServers:  true,
		SilenceServersWarning: true,
		Options: openapi3filter.Options{
			MultiError: true,
			// токен уже проверил Auth
			AuthenticationFunc: openapi3filter.NoopAuthenticationFunc,
		},
		ErrorHandlerWithOpts: func(_ context.Context, err error, w http.ResponseWriter, r *http.Request, opts nethttpmw.ErrorHandlerOpts) {
			if opts.StatusCode == http.StatusNotFound || opts.StatusCode == http.StatusMethodNotAllowed {
				apperr.Write(w, r, apperr.New(opts.StatusCode, api.NOTFOUND, "no such endpoint"))
				return
			}
			apperr.Write(w, r, apperr.Validation(fieldErrors(err, "")))
		},
	})
}

// fieldErrors раскладывает ошибку kin-openapi в список "поле: нарушение".
// Поле тела пишется как путь в JSON (items.1.quantity), параметр — своим именем (page).
func fieldErrors(err error, param string) []apperr.FieldError {
	switch typed := err.(type) {
	case openapi3.MultiError:
		var out []apperr.FieldError
		for _, inner := range typed {
			out = append(out, fieldErrors(inner, param)...)
		}
		return out
	case *openapi3filter.RequestError:
		if typed.Parameter != nil {
			param = typed.Parameter.Name
		}
		if typed.Err == nil {
			return []apperr.FieldError{{Field: param, Message: typed.Reason}}
		}
		return fieldErrors(typed.Err, param)
	case *openapi3.SchemaError:
		path := typed.JSONPointer()
		if param != "" {
			path = append([]string{param}, path...)
		}
		return []apperr.FieldError{{Field: strings.Join(path, "."), Message: typed.Reason}}
	}
	if inner := errors.Unwrap(err); inner != nil {
		return fieldErrors(inner, param)
	}
	return []apperr.FieldError{{Field: param, Message: err.Error()}}
}
