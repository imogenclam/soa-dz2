package handler

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"marketplace/internal/api"
	"marketplace/internal/apperr"
	"marketplace/internal/auth"
)

// Разрешённые переходы статуса заказа. Всё, чего тут нет, — INVALID_STATE_TRANSITION.
var transitions = map[api.OrderStatus][]api.OrderStatus{
	api.CREATED:        {api.PAYMENTPENDING, api.CANCELED},
	api.PAYMENTPENDING: {api.PAID, api.CANCELED},
	api.PAID:           {api.SHIPPED},
	api.SHIPPED:        {api.COMPLETED},
}

func canTransition(from, to api.OrderStatus) bool {
	return slices.Contains(transitions[from], to)
}

func invalidTransition(from, to api.OrderStatus) error {
	return apperr.New(http.StatusConflict, api.INVALIDSTATETRANSITION, "order status does not allow this").
		WithDetails(map[string]any{"from": from, "to": to})
}

// storedOrder — то, что нужно знать о заказе из базы перед его изменением.
type storedOrder struct {
	userID  int64
	status  api.OrderStatus
	promoID *int64
}

type reservedItem struct {
	productID int64
	quantity  int
	price     decimal.Decimal
}

func (s *Server) CreateOrder(ctx context.Context, request api.CreateOrderRequestObject) (api.CreateOrderResponseObject, error) {
	user, err := requireRole(ctx, api.USER, api.ADMIN)
	if err != nil {
		return nil, err
	}
	var order api.OrderResponse
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		if err := lockUser(ctx, tx, user.ID); err != nil {
			return err
		}
		// 1. частота
		if err := s.checkRateLimit(ctx, tx, user.ID, "CREATE_ORDER"); err != nil {
			return err
		}
		// 2. активный заказ
		var hasActive bool
		err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM orders
			WHERE user_id = $1 AND status IN ('CREATED', 'PAYMENT_PENDING'))`, user.ID).Scan(&hasActive)
		if err != nil {
			return err
		}
		if hasActive {
			return apperr.New(http.StatusConflict, api.ORDERHASACTIVE, "user already has an active order")
		}
		// 3-6. каталог, остатки, резерв, снапшот цен
		items, total, err := reserveItems(ctx, tx, request.Body.Items)
		if err != nil {
			return err
		}
		// 7. промокод
		discount := decimal.Zero
		var promoID *int64
		if request.Body.PromoCode != nil {
			var promo promoCode
			err := promo.scanFrom(tx.QueryRow(ctx, `SELECT `+promoColumns+` FROM promo_codes WHERE code = $1 FOR UPDATE`, *request.Body.PromoCode))
			if errors.Is(err, pgx.ErrNoRows) {
				return promoInvalid("not found")
			}
			if err != nil {
				return err
			}
			if !promo.inPeriod(time.Now()) {
				return promoInvalid("inactive or expired")
			}
			if promo.currentUses >= promo.maxUses {
				return promoInvalid("no uses left")
			}
			if total.LessThan(promo.minAmount) {
				return apperr.New(http.StatusUnprocessableEntity, api.PROMOCODEMINAMOUNT, "order total is below promo minimum").
					WithDetails(map[string]any{"min_order_amount": promo.minAmount, "total_amount": total})
			}
			discount = promo.discount(total)
			promoID = &promo.id
			if _, err := tx.Exec(ctx, `UPDATE promo_codes SET current_uses = current_uses + 1 WHERE id = $1`, promo.id); err != nil {
				return err
			}
		}
		// 8. сам заказ и запись операции
		var orderID int64
		err = tx.QueryRow(ctx, `INSERT INTO orders (user_id, status, promo_code_id, total_amount, discount_amount)
			VALUES ($1, 'CREATED', $2, $3, $4) RETURNING id`,
			user.ID, promoID, total.Sub(discount), discount).Scan(&orderID)
		if err != nil {
			return err
		}
		if err := insertItems(ctx, tx, orderID, items); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO user_operations (user_id, operation_type) VALUES ($1, 'CREATE_ORDER')`, user.ID); err != nil {
			return err
		}
		order, err = loadOrder(ctx, tx, orderID)
		return err
	})
	if err != nil {
		return nil, err
	}
	return api.CreateOrder201JSONResponse(order), nil
}

func (s *Server) GetOrder(ctx context.Context, request api.GetOrderRequestObject) (api.GetOrderResponseObject, error) {
	user, err := requireRole(ctx, api.USER, api.ADMIN)
	if err != nil {
		return nil, err
	}
	order, err := loadOrder(ctx, s.db, request.Id)
	if err != nil {
		return nil, err
	}
	if err := checkOrderOwner(user, order.UserId); err != nil {
		return nil, err
	}
	return api.GetOrder200JSONResponse(order), nil
}

func (s *Server) UpdateOrder(ctx context.Context, request api.UpdateOrderRequestObject) (api.UpdateOrderResponseObject, error) {
	user, err := requireRole(ctx, api.USER, api.ADMIN)
	if err != nil {
		return nil, err
	}
	var order api.OrderResponse
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		stored, err := lockOrder(ctx, tx, request.Id)
		if err != nil {
			return err
		}
		// 1. владелец, 2. статус, 3. частота. Если меняет админ, лимит и операция всё равно на владельце.
		if err := checkOrderOwner(user, stored.userID); err != nil {
			return err
		}
		if err := lockUser(ctx, tx, stored.userID); err != nil {
			return err
		}
		if stored.status != api.CREATED {
			return apperr.New(http.StatusConflict, api.INVALIDSTATETRANSITION, "order can be changed only in CREATED").
				WithDetails(map[string]any{"status": stored.status})
		}
		if err := s.checkRateLimit(ctx, tx, stored.userID, "UPDATE_ORDER"); err != nil {
			return err
		}
		// 4. вернуть старые остатки и убрать старые позиции
		if err := returnStock(ctx, tx, request.Id); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM order_items WHERE order_id = $1`, request.Id); err != nil {
			return err
		}
		// 5. новые позиции
		items, total, err := reserveItems(ctx, tx, request.Body.Items)
		if err != nil {
			return err
		}
		// 6. промокод проверяем заново; если больше не подходит, снимаем и возвращаем использование
		discount := decimal.Zero
		promoID := stored.promoID
		if promoID != nil {
			var promo promoCode
			if err := promo.scanFrom(tx.QueryRow(ctx, `SELECT `+promoColumns+` FROM promo_codes WHERE id = $1 FOR UPDATE`, *promoID)); err != nil {
				return err
			}
			if promo.inPeriod(time.Now()) && !total.LessThan(promo.minAmount) {
				discount = promo.discount(total)
			} else {
				promoID = nil
				if _, err := tx.Exec(ctx, `UPDATE promo_codes SET current_uses = current_uses - 1 WHERE id = $1`, promo.id); err != nil {
					return err
				}
			}
		}
		_, err = tx.Exec(ctx, `UPDATE orders SET promo_code_id = $2, total_amount = $3, discount_amount = $4 WHERE id = $1`,
			request.Id, promoID, total.Sub(discount), discount)
		if err != nil {
			return err
		}
		if err := insertItems(ctx, tx, request.Id, items); err != nil {
			return err
		}
		// 7. запись операции
		if _, err := tx.Exec(ctx, `INSERT INTO user_operations (user_id, operation_type) VALUES ($1, 'UPDATE_ORDER')`, stored.userID); err != nil {
			return err
		}
		order, err = loadOrder(ctx, tx, request.Id)
		return err
	})
	if err != nil {
		return nil, err
	}
	return api.UpdateOrder200JSONResponse(order), nil
}

func (s *Server) CancelOrder(ctx context.Context, request api.CancelOrderRequestObject) (api.CancelOrderResponseObject, error) {
	user, err := requireRole(ctx, api.USER, api.ADMIN)
	if err != nil {
		return nil, err
	}
	var order api.OrderResponse
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		stored, err := lockOrder(ctx, tx, request.Id)
		if err != nil {
			return err
		}
		if err := checkOrderOwner(user, stored.userID); err != nil {
			return err
		}
		if !canTransition(stored.status, api.CANCELED) {
			return invalidTransition(stored.status, api.CANCELED)
		}
		if err := returnStock(ctx, tx, request.Id); err != nil {
			return err
		}
		if stored.promoID != nil {
			if _, err := tx.Exec(ctx, `UPDATE promo_codes SET current_uses = current_uses - 1 WHERE id = $1`, *stored.promoID); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE orders SET status = 'CANCELED' WHERE id = $1`, request.Id); err != nil {
			return err
		}
		order, err = loadOrder(ctx, tx, request.Id)
		return err
	})
	if err != nil {
		return nil, err
	}
	return api.CancelOrder200JSONResponse(order), nil
}

// lockUser выстраивает операции одного пользователя в очередь до конца транзакции.
// Без этого два одновременных POST /orders оба прошли бы проверки частоты и активного заказа.
func lockUser(ctx context.Context, tx pgx.Tx, userID int64) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, userID)
	return err
}

func (s *Server) checkRateLimit(ctx context.Context, tx pgx.Tx, userID int64, operation string) error {
	var lastAt time.Time
	err := tx.QueryRow(ctx, `SELECT created_at FROM user_operations
		WHERE user_id = $1 AND operation_type = $2 ORDER BY created_at DESC LIMIT 1`, userID, operation).Scan(&lastAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if wait := s.orderLimit - time.Since(lastAt); wait > 0 {
		return apperr.New(http.StatusTooManyRequests, api.ORDERLIMITEXCEEDED, "too many order operations").
			WithDetails(map[string]any{"operation": operation, "retry_after_seconds": int(wait.Seconds()) + 1})
	}
	return nil
}

func checkOrderOwner(user auth.User, ownerID int64) error {
	if user.Role != api.ADMIN && user.ID != ownerID {
		return apperr.New(http.StatusForbidden, api.ORDEROWNERSHIPVIOLATION, "order belongs to another user")
	}
	return nil
}

func orderNotFound(id int64) error {
	return apperr.New(http.StatusNotFound, api.ORDERNOTFOUND, "order not found").
		WithDetails(map[string]any{"order_id": id})
}

// lockOrder читает заказ и блокирует его строку до конца транзакции.
func lockOrder(ctx context.Context, tx pgx.Tx, id int64) (storedOrder, error) {
	var stored storedOrder
	err := tx.QueryRow(ctx, `SELECT user_id, status, promo_code_id FROM orders WHERE id = $1 FOR UPDATE`, id).
		Scan(&stored.userID, &stored.status, &stored.promoID)
	if errors.Is(err, pgx.ErrNoRows) {
		return stored, orderNotFound(id)
	}
	return stored, err
}

// reserveItems проверяет каталог и остатки, списывает остатки и запоминает цены.
// Возвращает позиции и сумму без скидки.
func reserveItems(ctx context.Context, tx pgx.Tx, requested api.OrderItems) ([]reservedItem, decimal.Decimal, error) {
	// один товар дважды в запросе складываем в одну позицию
	quantities := map[int64]int{}
	var productIDs []int64
	for _, item := range requested {
		if _, seen := quantities[item.ProductId]; !seen {
			productIDs = append(productIDs, item.ProductId)
		}
		quantities[item.ProductId] += item.Quantity
	}

	type product struct {
		price  decimal.Decimal
		stock  int
		status api.ProductStatus
	}
	// FOR UPDATE: никто другой не спишет эти остатки, пока мы не закончим. ORDER BY id против взаимных блокировок.
	rows, err := tx.Query(ctx, `SELECT id, price, stock, status FROM products WHERE id = ANY($1) ORDER BY id FOR UPDATE`, productIDs)
	if err != nil {
		return nil, decimal.Zero, err
	}
	products := map[int64]product{}
	for rows.Next() {
		var id int64
		var found product
		if err := rows.Scan(&id, &found.price, &found.stock, &found.status); err != nil {
			return nil, decimal.Zero, err
		}
		products[id] = found
	}
	if err := rows.Err(); err != nil {
		return nil, decimal.Zero, err
	}

	for _, id := range productIDs {
		found, ok := products[id]
		if !ok {
			return nil, decimal.Zero, productNotFound(id)
		}
		if found.status != api.ACTIVE {
			return nil, decimal.Zero, apperr.New(http.StatusConflict, api.PRODUCTINACTIVE, "product is not active").
				WithDetails(map[string]any{"product_id": id, "status": found.status})
		}
	}

	var shortages []map[string]any
	for _, id := range productIDs {
		if found := products[id]; found.stock < quantities[id] {
			shortages = append(shortages, map[string]any{"product_id": id, "requested": quantities[id], "available": found.stock})
		}
	}
	if len(shortages) > 0 {
		return nil, decimal.Zero, apperr.New(http.StatusConflict, api.INSUFFICIENTSTOCK, "not enough stock").
			WithDetails(map[string]any{"items": shortages})
	}

	items := make([]reservedItem, 0, len(productIDs))
	total := decimal.Zero
	for _, id := range productIDs {
		if _, err := tx.Exec(ctx, `UPDATE products SET stock = stock - $2 WHERE id = $1`, id, quantities[id]); err != nil {
			return nil, decimal.Zero, err
		}
		price := products[id].price
		items = append(items, reservedItem{productID: id, quantity: quantities[id], price: price})
		total = total.Add(price.Mul(decimal.NewFromInt(int64(quantities[id]))))
	}
	return items, total, nil
}

func insertItems(ctx context.Context, tx pgx.Tx, orderID int64, items []reservedItem) error {
	for _, item := range items {
		_, err := tx.Exec(ctx, `INSERT INTO order_items (order_id, product_id, quantity, price_at_order) VALUES ($1, $2, $3, $4)`,
			orderID, item.productID, item.quantity, item.price)
		if err != nil {
			return err
		}
	}
	return nil
}

// returnStock возвращает на склад всё, что зарезервировано позициями заказа.
func returnStock(ctx context.Context, tx pgx.Tx, orderID int64) error {
	_, err := tx.Exec(ctx, `UPDATE products p SET stock = p.stock + oi.quantity
		FROM order_items oi WHERE oi.order_id = $1 AND p.id = oi.product_id`, orderID)
	return err
}

func loadOrder(ctx context.Context, db querier, id int64) (api.OrderResponse, error) {
	var order api.OrderResponse
	err := db.QueryRow(ctx, `SELECT id, user_id, status, promo_code_id, total_amount, discount_amount, created_at, updated_at
		FROM orders WHERE id = $1`, id).
		Scan(&order.Id, &order.UserId, &order.Status, &order.PromoCodeId, &order.TotalAmount, &order.DiscountAmount,
			&order.CreatedAt, &order.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return order, orderNotFound(id)
	}
	if err != nil {
		return order, err
	}
	rows, err := db.Query(ctx, `SELECT product_id, quantity, price_at_order FROM order_items WHERE order_id = $1 ORDER BY id`, id)
	if err != nil {
		return order, err
	}
	order.Items = []api.OrderItemResponse{}
	for rows.Next() {
		var item api.OrderItemResponse
		if err := rows.Scan(&item.ProductId, &item.Quantity, &item.PriceAtOrder); err != nil {
			return order, err
		}
		order.Items = append(order.Items, item)
	}
	return order, rows.Err()
}
