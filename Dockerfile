# syntax=docker/dockerfile:1

# --- Derleme asamasi ---
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# modernc.org/sqlite saf Go oldugundan CGO kapali, statik binary uretilir.
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /out/bulbi-backend ./cmd/server

# --- Calisma asamasi ---
FROM alpine:3.21
RUN apk add --no-cache ca-certificates tzdata \
    && adduser -D -u 10001 app
WORKDIR /app
COPY --from=build /out/bulbi-backend /app/bulbi-backend
COPY data/content.json /app/data/content.json
# Yuklenen bildirim gorselleri icin yazilabilir dizin (app kullanicisina ait).
# Bos named volume ilk baglandiginda Docker bu sahipligi kopyalar.
RUN mkdir -p /app/data/uploads && chown -R app:app /app/data
USER app
ENV ADDR=":8080" \
    CONTENT_PATH="/app/data/content.json"
EXPOSE 8080
ENTRYPOINT ["/app/bulbi-backend"]
