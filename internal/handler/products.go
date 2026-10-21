package handler

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"

	"marketplace/internal/api"
	"marketplace/internal/apperr"
	"marketplace/internal/auth"
)

const productColumns = `id, name, description, price, stock, category, status, seller_id, created_at, updated_at`

func scanProduct(row pgx.Row) (api.ProductResponse, error) {
	var product api.ProductResponse
	err := row.Scan(&product.Id, &product.Name, &product.Description, &product.Price, &product.Stock, &product.Category,
		&product.Status, &product.SellerId, &product.CreatedAt, &product.UpdatedAt)
	return product, err
}

func productNotFound(id int64) error {
	return apperr.New(http.StatusNotFound, api.PRODUCTNOTFOUND, "product not found").
		WithDetails(map[string]any{"product_id": id})
}

func (s *Server) ListProducts(ctx context.Context, request api.ListProductsRequestObject) (api.ListProductsResponseObject, error) {
	page, size := 0, 20
	if request.Params.Page != nil {
		page = *request.Params.Page
	}
	if request.Params.Size != nil {
		size = *request.Params.Size
	}

	var conditions []string
	var args []any
	if request.Params.Status != nil {
		args = append(args, string(*request.Params.Status))
		conditions = append(conditions, fmt.Sprintf("status = $%d", len(args)))
	}
	if request.Params.Category != nil {
		args = append(args, *request.Params.Category)
		conditions = append(conditions, fmt.Sprintf("category = $%d", len(args)))
	}
	where := ""
	if len(conditions) > 0 {
		where = " WHERE " + strings.Join(conditions, " AND ")
	}

	var total int64
	if err := s.db.QueryRow(ctx, "SELECT count(*) FROM products"+where, args...).Scan(&total); err != nil {
		return nil, err
	}

	args = append(args, size, page*size)
	rows, err := s.db.Query(ctx, fmt.Sprintf("SELECT %s FROM products%s ORDER BY id LIMIT $%d OFFSET $%d",
		productColumns, where, len(args)-1, len(args)), args...)
	if err != nil {
		return nil, err
	}
	products := []api.ProductResponse{}
	for rows.Next() {
		product, err := scanProduct(rows)
		if err != nil {
			return nil, err
		}
		products = append(products, product)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return api.ListProducts200JSONResponse{Items: products, TotalElements: total, Page: page, Size: size}, nil
}

func (s *Server) GetProduct(ctx context.Context, request api.GetProductRequestObject) (api.GetProductResponseObject, error) {
	product, err := scanProduct(s.db.QueryRow(ctx, "SELECT "+productColumns+" FROM products WHERE id = $1", request.Id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, productNotFound(request.Id)
	}
	if err != nil {
		return nil, err
	}
	return api.GetProduct200JSONResponse(product), nil
}

func (s *Server) CreateProduct(ctx context.Context, request api.CreateProductRequestObject) (api.CreateProductResponseObject, error) {
	user, err := requireRole(ctx, api.SELLER, api.ADMIN)
	if err != nil {
		return nil, err
	}
	body := request.Body
	product, err := scanProduct(s.db.QueryRow(ctx,
		`INSERT INTO products (name, description, price, stock, category, status, seller_id)
		 VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING `+productColumns,
		body.Name, body.Description, body.Price, body.Stock, body.Category, string(body.Status), user.ID))
	if err != nil {
		return nil, err
	}
	return api.CreateProduct201JSONResponse(product), nil
}

// checkProductOwner: ADMIN может всё, SELLER только свои товары.
func (s *Server) checkProductOwner(ctx context.Context, user auth.User, id int64) error {
	var sellerID int64
	err := s.db.QueryRow(ctx, `SELECT seller_id FROM products WHERE id = $1`, id).Scan(&sellerID)
	if errors.Is(err, pgx.ErrNoRows) {
		return productNotFound(id)
	}
	if err != nil {
		return err
	}
	if user.Role == api.SELLER && sellerID != user.ID {
		return apperr.AccessDenied()
	}
	return nil
}

func (s *Server) UpdateProduct(ctx context.Context, request api.UpdateProductRequestObject) (api.UpdateProductResponseObject, error) {
	user, err := requireRole(ctx, api.SELLER, api.ADMIN)
	if err != nil {
		return nil, err
	}
	if err := s.checkProductOwner(ctx, user, request.Id); err != nil {
		return nil, err
	}
	body := request.Body
	product, err := scanProduct(s.db.QueryRow(ctx,
		`UPDATE products SET name = $2, description = $3, price = $4, stock = $5, category = $6, status = $7
		 WHERE id = $1 RETURNING `+productColumns,
		request.Id, body.Name, body.Description, body.Price, body.Stock, body.Category, string(body.Status)))
	if err != nil {
		return nil, err
	}
	return api.UpdateProduct200JSONResponse(product), nil
}

// DeleteProduct не удаляет строку, а переводит товар в ARCHIVED: на него ссылаются старые заказы.
func (s *Server) DeleteProduct(ctx context.Context, request api.DeleteProductRequestObject) (api.DeleteProductResponseObject, error) {
	user, err := requireRole(ctx, api.SELLER, api.ADMIN)
	if err != nil {
		return nil, err
	}
	if err := s.checkProductOwner(ctx, user, request.Id); err != nil {
		return nil, err
	}
	product, err := scanProduct(s.db.QueryRow(ctx,
		`UPDATE products SET status = 'ARCHIVED' WHERE id = $1 RETURNING `+productColumns, request.Id))
	if err != nil {
		return nil, err
	}
	return api.DeleteProduct200JSONResponse(product), nil
}
