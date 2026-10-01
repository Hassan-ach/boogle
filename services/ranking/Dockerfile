FROM python:3.12-alpine AS builder

WORKDIR /app

RUN apk add --no-cache \
    build-base \
    linux-headers \
    musl-dev \
    zlib-dev \
    libffi-dev \
    postgresql-dev \
    openssl-dev \
    bash \
    curl \
    git

RUN pip install --upgrade pip
COPY requirements.txt .
RUN pip install --prefix=/install -r requirements.txt

# Set both PATH and PYTHONPATH
ENV PATH="/install/bin:${PATH}"
ENV PYTHONPATH="/install/lib/python3.12/site-packages:${PYTHONPATH}"

COPY . .
RUN pyinstaller --onefile src/main.py

FROM alpine:3.23

RUN addgroup -S app -g 10001 \
 && adduser -S app -u 10001 -G app \
 && apk add --no-cache zlib libstdc++ libpq

WORKDIR /app

COPY --from=builder --chown=app:app /app/dist/main .
COPY --chown=app:app --chmod=0555 entrypoint.sh /entrypoint.sh

USER 10001:10001

HEALTHCHECK --interval=30s --timeout=5s --start-period=30s --retries=3 \
  CMD pgrep -x main > /dev/null || exit 1

ENTRYPOINT ["/entrypoint.sh"]
