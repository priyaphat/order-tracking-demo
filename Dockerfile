FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/api ./cmd/api

FROM alpine:3.22
RUN addgroup -S app && adduser -S app -G app
WORKDIR /app
COPY --from=build /out/api /app/api
COPY db/migrations /app/db/migrations
COPY openapi.yaml /app/openapi.yaml
USER app
EXPOSE 8080
ENTRYPOINT ["/app/api"]

