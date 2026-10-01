FROM golang:1.27.1-alpine3.23 AS go-builder

WORKDIR /app

RUN apk add --no-cache \
    build-base \
    just

RUN go install github.com/a-h/templ/cmd/templ@latest

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN just build

FROM alpine:3.23

RUN addgroup -S app -g 10001 \
    && adduser -S app -u 10001 -G app

WORKDIR /app

RUN mkdir -p /app/logs && chown app:app /app/logs

COPY --from=go-builder --chown=app:app /app/spider .
COPY --chown=app:app --chmod=0555 entrypoint.sh /entrypoint.sh

USER 10001:10001

HEALTHCHECK --interval=30s --timeout=5s --start-period=30s --retries=3 \
  CMD pgrep -x spider > /dev/null || exit 1

ENTRYPOINT ["/entrypoint.sh"]
