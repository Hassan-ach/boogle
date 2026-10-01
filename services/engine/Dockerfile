FROM node:current-alpine3.23 AS builder

WORKDIR /app

COPY package*.json ./

RUN npm ci

COPY . .

RUN apk add --no-cache just

RUN just tailwind-build-prod
RUN just js-prod


FROM golang:1.27.1-alpine3.23 AS go-builder

WORKDIR /app

RUN apk add --no-cache \
    build-base \
    aspell-dev \
    just

RUN go install github.com/a-h/templ/cmd/templ@v0.3.1020

COPY go.mod go.sum ./
RUN go mod download

COPY . .

COPY --from=builder /app/public ./public

RUN just build-emb

FROM alpine:3.23

RUN apk add --no-cache aspell aspell-en \
 && addgroup -S app -g 10001 \
 && adduser -S app -u 10001 -G app

WORKDIR /app

COPY --from=go-builder --chown=app:app /app/boogle .
COPY --from=builder --chown=app:app /app/public ./public
COPY --chown=app:app entrypoint.sh /entrypoint.sh
RUN chmod 0555 /entrypoint.sh

USER 10001:10001

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD wget -q --spider http://localhost:1323/health || exit 1

EXPOSE 1323
ENTRYPOINT ["/entrypoint.sh"]
