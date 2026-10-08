# LangSpeed server image.
# The build context is the repository root so this Dockerfile can see both the
# Go module (server/) and the static frontend (web/).

# ---------- build stage ----------
FROM golang:1.27-alpine AS build
WORKDIR /src

# Warm the module cache before copying sources (better layer caching).
COPY server/go.mod server/go.sum ./
RUN go mod download

COPY server/ ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/langspeed-server ./cmd/server

# ---------- runtime stage ----------
FROM alpine:3.22
RUN apk add --no-cache ca-certificates \
 && adduser -D -H -u 10001 langspeed

WORKDIR /app
COPY --from=build /out/langspeed-server /usr/local/bin/langspeed-server
COPY web/ /app/web/
ENV STATIC_DIR=/app/web

USER langspeed
EXPOSE 3000

# Smoke-check that the server answers on its health endpoint (R1).
HEALTHCHECK --interval=15s --timeout=5s --start-period=20s --retries=3 \
  CMD wget -q -O /dev/null "http://127.0.0.1:${SERVER_PORT:-3000}/healthz" || exit 1

ENTRYPOINT ["/usr/local/bin/langspeed-server"]
