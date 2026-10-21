package handler

import (
	"context"
	"net/http"
	"time"

	"github.com/shopspring/decimal"

	"marketplace/internal/api"
	"marketplace/internal/apperr"
)

type promoCode struct {
	id            int64
	discountType  api.DiscountType
	discountValue decimal.Decimal
	minAmount     decimal.Decimal
	maxUses       int
	currentUses   int
	validFrom     time.Time
	validUntil    time.Time
	active        bool
}

const promoColumns = `id, discount_type, discount_value, min_order_amount, max_uses, current_uses, valid_from, valid_until, active`

func (promo *promoCode) scanFrom(row interface{ Scan(...any) error }) error {
	return row.Scan(&promo.id, &promo.discountType, &promo.discountValue, &promo.minAmount, &promo.maxUses,
		&promo.currentUses, &promo.validFrom, &promo.validUntil, &promo.active)
}

func (promo *promoCode) inPeriod(now time.Time) bool {
	return promo.active && !now.Before(promo.validFrom) && !now.After(promo.validUntil)
}

// discount: процентная скидка больше 70% суммы не применяется (цена 100%), фиксированная не больше суммы.
func (promo *promoCode) discount(total decimal.Decimal) decimal.Decimal {
	var discount decimal.Decimal
	switch promo.discountType {
	case api.PERCENTAGE:
		discount = total.Mul(promo.discountValue).Div(decimal.NewFromInt(100))
		if discount.GreaterThan(total.Mul(decimal.RequireFromString("0.7"))) {
			discount = decimal.Zero
		}
	case api.FIXEDAMOUNT:
		discount = decimal.Min(promo.discountValue, total)
	}
	return discount.Round(2)
}

func promoInvalid(reason string) error {
	return apperr.New(http.StatusUnprocessableEntity, api.PROMOCODEINVALID, "promo code cannot be used").
		WithDetails(map[string]any{"reason": reason})
}

func (s *Server) CreatePromoCode(ctx context.Context, request api.CreatePromoCodeRequestObject) (api.CreatePromoCodeResponseObject, error) {
	if _, err := requireRole(ctx, api.SELLER, api.ADMIN); err != nil {
		return nil, err
	}
	body := request.Body
	if !body.ValidUntil.After(body.ValidFrom) {
		return nil, apperr.Validation([]apperr.FieldError{{Field: "valid_until", Message: "must be after valid_from"}})
	}
	minAmount := decimal.Zero
	if body.MinOrderAmount != nil {
		minAmount = *body.MinOrderAmount
	}

	var created api.PromoCodeResponse
	err := s.db.QueryRow(ctx,
		`INSERT INTO promo_codes (code, discount_type, discount_value, min_order_amount, max_uses, valid_from, valid_until)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)
		 RETURNING id, code, discount_type, discount_value, min_order_amount, max_uses, current_uses, valid_from, valid_until, active`,
		body.Code, string(body.DiscountType), body.DiscountValue, minAmount, body.MaxUses, body.ValidFrom, body.ValidUntil).
		Scan(&created.Id, &created.Code, &created.DiscountType, &created.DiscountValue, &created.MinOrderAmount,
			&created.MaxUses, &created.CurrentUses, &created.ValidFrom, &created.ValidUntil, &created.Active)
	if isUniqueViolation(err) {
		return nil, apperr.New(http.StatusConflict, api.PROMOCODEEXISTS, "promo code already exists")
	}
	if err != nil {
		return nil, err
	}
	return api.CreatePromoCode201JSONResponse(created), nil
}
