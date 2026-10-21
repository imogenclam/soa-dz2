FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# код из openapi.yaml генерируется при каждой сборке, в git его нет
RUN go generate ./... && CGO_ENABLED=0 go build -o /out/marketplace ./cmd/server

FROM alpine:3.22
COPY --from=build /out/marketplace /usr/local/bin/marketplace
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/marketplace"]
