package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"
	pgxdecimal "github.com/jackc/pgx-shopspring-decimal"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"marketplace/internal/api"
	"marketplace/internal/apperr"
	"marketplace/internal/auth"
	"marketplace/internal/handler"
	"marketplace/internal/middleware"
)

func main() {
	// без этого decimal уходит в JSON строкой "12.50", а в контракте number
	decimal.MarshalJSONWithoutQuotes = true

	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		// время пишем сами в поле timestamp
		ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			if a.Key == slog.TimeKey {
				return slog.Attr{}
			}
			return a
		},
	}))
	slog.SetDefault(log)

	if err := run(log); err != nil {
		log.Error("server stopped", "error", err.Error())
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	secret := os.Getenv("JWT_SECRET")
	if secret == "" {
		return errors.New("JWT_SECRET is empty")
	}
	rateLimitMinutes, err := strconv.ParseFloat(env("ORDER_RATE_LIMIT_MINUTES", "1"), 64)
	if err != nil {
		return err
	}

	poolConfig, err := pgxpool.ParseConfig(os.Getenv("DATABASE_URL"))
	if err != nil {
		return err
	}
	poolConfig.AfterConnect = func(_ context.Context, conn *pgx.Conn) error {
		pgxdecimal.Register(conn.TypeMap())
		return nil
	}
	db, err := pgxpool.NewWithConfig(context.Background(), poolConfig)
	if err != nil {
		return err
	}
	defer db.Close()

	spec, err := api.GetSwagger()
	if err != nil {
		return err
	}

	tokens := auth.NewTokens(secret)
	handlers := handler.New(db, tokens, time.Duration(rateLimitMinutes*float64(time.Minute)))
	writeErr := func(w http.ResponseWriter, r *http.Request, err error) { apperr.Write(w, r, err) }
	badRequest := func(w http.ResponseWriter, r *http.Request, err error) {
		apperr.Write(w, r, apperr.Validation([]apperr.FieldError{{Message: err.Error()}}))
	}
	strictHandler := api.NewStrictHandlerWithOptions(handlers, nil, api.StrictHTTPServerOptions{
		RequestErrorHandlerFunc:  badRequest,
		ResponseErrorHandlerFunc: writeErr,
	})

	router := chi.NewRouter()
	router.Use(middleware.Logging(log), chimw.Recoverer, middleware.Auth(tokens), middleware.Validate(spec))
	api.HandlerWithOptions(strictHandler, api.ChiServerOptions{BaseRouter: router, ErrorHandlerFunc: badRequest})

	addr := ":" + env("PORT", "8080")
	log.Info("listening", "addr", addr, "order_rate_limit_minutes", rateLimitMinutes)
	httpServer := &http.Server{
		Addr:              addr,
		Handler:           router,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
	}
	return httpServer.ListenAndServe()
}

func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
