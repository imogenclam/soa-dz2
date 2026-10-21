// Package handler реализует сгенерированный api.StrictServerInterface.
package handler

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"marketplace/internal/api"
	"marketplace/internal/apperr"
	"marketplace/internal/auth"
)

type Server struct {
	db         *pgxpool.Pool
	tokens     *auth.Tokens
	orderLimit time.Duration // пауза между созданиями (и между обновлениями) заказа
}

var _ api.StrictServerInterface = (*Server)(nil)

func New(db *pgxpool.Pool, tokens *auth.Tokens, orderLimit time.Duration) *Server {
	return &Server{db: db, tokens: tokens, orderLimit: orderLimit}
}

// querier — общее у пула и транзакции, чтобы одни и те же функции работали и там, и там.
type querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// inTx: всё внутри fn либо применяется целиком, либо откатывается.
func (s *Server) inTx(ctx context.Context, fn func(tx pgx.Tx) error) error {
	return pgx.BeginFunc(ctx, s.db, fn)
}

// requireRole возвращает пользователя запроса или 403, если его роли нет в списке.
func requireRole(ctx context.Context, roles ...api.Role) (auth.User, error) {
	user, ok := auth.FromContext(ctx)
	if !ok || !slices.Contains(roles, user.Role) {
		return auth.User{}, apperr.AccessDenied()
	}
	return user, nil
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
