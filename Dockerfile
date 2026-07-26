# Pure-Go build (modernc.org/sqlite, no cgo), so the binary is static and the
# runtime image only has to supply ffmpeg.
FROM golang:1.26-alpine AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/proxystream ./cmd/proxystream

FROM alpine:3.21

RUN apk add --no-cache ffmpeg ca-certificates tzdata \
 && adduser -D -u 1000 -h /app proxystream

WORKDIR /app
COPY --from=build /out/proxystream /usr/local/bin/proxystream

# Everything that must survive a container rebuild lives here.
RUN mkdir -p /data/segments /data/subs /data/thumbs && chown -R proxystream:proxystream /data
VOLUME ["/data"]

ENV DB_PATH=/data/app.db \
    CACHE_DIR=/data/segments \
    SUBTITLE_DIR=/data/subs \
    THUMB_DIR=/data/thumbs \
    HTTP_ADDR=0.0.0.0:8080 \
    INTERNAL_ADDR=127.0.0.1:8081

USER proxystream
EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD wget -q -O- http://127.0.0.1:8080/health || exit 1

ENTRYPOINT ["proxystream"]
