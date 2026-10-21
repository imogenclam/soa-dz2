package handler

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/bcrypt"

	"marketplace/internal/api"
	"marketplace/internal/apperr"
	"marketplace/internal/auth"
)

func (s *Server) Register(ctx context.Context, request api.RegisterRequestObject) (api.RegisterResponseObject, error) {
	role := api.USER
	if request.Body.Role != nil {
		role = *request.Body.Role
	}
	passwordHash, err := bcrypt.GenerateFromPassword([]byte(request.Body.Password), bcrypt.DefaultCost)
	if err != nil {
		return nil, err
	}
	email := strings.ToLower(string(request.Body.Email))

	var id int64
	err = s.db.QueryRow(ctx,
		`INSERT INTO users (email, password_hash, role) VALUES ($1, $2, $3) RETURNING id`,
		email, string(passwordHash), string(role)).Scan(&id)
	if isUniqueViolation(err) {
		return nil, apperr.New(http.StatusConflict, api.EMAILTAKEN, "email already registered")
	}
	if err != nil {
		return nil, err
	}
	return api.Register201JSONResponse{Id: id, Email: email, Role: role}, nil
}

func (s *Server) Login(ctx context.Context, request api.LoginRequestObject) (api.LoginResponseObject, error) {
	var user auth.User
	var passwordHash string
	err := s.db.QueryRow(ctx, `SELECT id, role, password_hash FROM users WHERE email = $1`,
		strings.ToLower(string(request.Body.Email))).Scan(&user.ID, &user.Role, &passwordHash)
	// одна и та же ошибка для "нет такого" и "не тот пароль", чтобы не подсказывать, какие почты есть
	wrongPassword := err == nil && bcrypt.CompareHashAndPassword([]byte(passwordHash), []byte(request.Body.Password)) != nil
	if errors.Is(err, pgx.ErrNoRows) || wrongPassword {
		return nil, apperr.New(http.StatusUnauthorized, api.INVALIDCREDENTIALS, "wrong email or password")
	}
	if err != nil {
		return nil, err
	}
	tokenPair, err := s.tokens.Issue(user)
	if err != nil {
		return nil, err
	}
	return api.Login200JSONResponse(tokenPair), nil
}

func (s *Server) Refresh(ctx context.Context, request api.RefreshRequestObject) (api.RefreshResponseObject, error) {
	user, err := s.tokens.ParseRefresh(request.Body.RefreshToken)
	if err != nil {
		return nil, err
	}
	// роль берём из базы: вдруг её поменяли после выдачи refresh токена
	err = s.db.QueryRow(ctx, `SELECT role FROM users WHERE id = $1`, user.ID).Scan(&user.Role)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, apperr.New(http.StatusUnauthorized, api.REFRESHTOKENINVALID, "user not found")
	}
	if err != nil {
		return nil, err
	}
	tokenPair, err := s.tokens.Issue(user)
	if err != nil {
		return nil, err
	}
	return api.Refresh200JSONResponse(tokenPair), nil
}
