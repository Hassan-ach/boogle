FROM rust:alpine3.23 AS builder

RUN apk add --no-cache musl-dev musl-utils build-base

WORKDIR /app

COPY . .

ENV SQLX_OFFLINE=true
# Baseline x86-64-v2 instead of -C target-cpu=native: the image is built in CI
# and may run on an older host, where native instructions abort with SIGILL (M-23).
ENV RUSTFLAGS="-C target-cpu=x86-64-v2"

RUN rustup target add x86_64-unknown-linux-musl
RUN cargo build --release --target x86_64-unknown-linux-musl

FROM alpine:3.23

RUN addgroup -S app -g 10001 \
 && adduser -S app -u 10001 -G app

WORKDIR /app

RUN mkdir -p /app/logs && chown app:app /app/logs

COPY --from=builder --chown=app:app /app/target/x86_64-unknown-linux-musl/release/indexer .
COPY --chown=app:app --chmod=0555 entrypoint.sh /entrypoint.sh

USER 10001:10001

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD pgrep -x indexer > /dev/null || exit 1

ENTRYPOINT ["/entrypoint.sh"]
