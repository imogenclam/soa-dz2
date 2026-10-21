// Package api собирается из api/openapi.yaml. Код генерируется командой go generate ./...
package api

//go:generate go tool oapi-codegen -config cfg.yaml ../../api/openapi.yaml
