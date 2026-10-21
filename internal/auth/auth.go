// Package auth выдаёт и проверяет JWT и кладёт пользователя запроса в контекст.
package auth

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"marketplace/internal/api"
	"marketplace/internal/apperr"
)

const (
	AccessTTL  = 15 * time.Minute
	RefreshTTL = 7 * 24 * time.Hour

	typeAccess  = "access"
	typeRefresh = "refresh"
)

type User struct {
	ID   int64
	Role api.Role
}

type tokenClaims struct {
	Role api.Role `json:"role"`
	Type string   `json:"typ"`
	jwt.RegisteredClaims
}

type Tokens struct {
	secret []byte
}

func NewTokens(secret string) *Tokens {
	return &Tokens{secret: []byte(secret)}
}

func (t *Tokens) Issue(user User) (api.TokenPair, error) {
	access, err := t.sign(user, typeAccess, AccessTTL)
	if err != nil {
		return api.TokenPair{}, err
	}
	refresh, err := t.sign(user, typeRefresh, RefreshTTL)
	if err != nil {
		return api.TokenPair{}, err
	}
	return api.TokenPair{
		AccessToken:  access,
		RefreshToken: refresh,
		TokenType:    "Bearer",
		ExpiresIn:    int(AccessTTL.Seconds()),
	}, nil
}

func (t *Tokens) sign(user User, tokenType string, ttl time.Duration) (string, error) {
	now := time.Now()
	claims := tokenClaims{
		Role: user.Role,
		Type: tokenType,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   strconv.FormatInt(user.ID, 10),
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(ttl)),
		},
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(t.secret)
}

// ParseAccess проверяет access токен. Refresh токен сюда не подходит.
func (t *Tokens) ParseAccess(rawToken string) (User, error) {
	user, err := t.parse(rawToken, typeAccess)
	if errors.Is(err, jwt.ErrTokenExpired) {
		return User{}, apperr.New(http.StatusUnauthorized, api.TOKENEXPIRED, "access token expired")
	}
	if err != nil {
		return User{}, apperr.New(http.StatusUnauthorized, api.TOKENINVALID, "access token invalid")
	}
	return user, nil
}

func (t *Tokens) ParseRefresh(rawToken string) (User, error) {
	user, err := t.parse(rawToken, typeRefresh)
	if err != nil {
		return User{}, apperr.New(http.StatusUnauthorized, api.REFRESHTOKENINVALID, "refresh token invalid or expired")
	}
	return user, nil
}

func (t *Tokens) parse(rawToken, tokenType string) (User, error) {
	var claims tokenClaims
	_, err := jwt.ParseWithClaims(rawToken, &claims, func(*jwt.Token) (any, error) { return t.secret, nil },
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}))
	if err != nil {
		return User{}, err
	}
	if claims.Type != tokenType {
		return User{}, errors.New("wrong token type")
	}
	id, err := strconv.ParseInt(claims.Subject, 10, 64)
	if err != nil {
		return User{}, err
	}
	return User{ID: id, Role: claims.Role}, nil
}

type ctxKey struct{}

func WithUser(ctx context.Context, user User) context.Context {
	return context.WithValue(ctx, ctxKey{}, user)
}

func FromContext(ctx context.Context) (User, bool) {
	user, ok := ctx.Value(ctxKey{}).(User)
	return user, ok
}
