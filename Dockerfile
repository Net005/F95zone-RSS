# ---- build ----
FROM golang:1.26-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /f95zone .

# ---- runtime (Chromium is only used when fetch_mode = browser) ----
FROM debian:bookworm-slim
LABEL org.opencontainers.image.source="https://github.com/Net005/F95zone-RSS" \
      org.opencontainers.image.description="F95Zone RSS enricher with web control panel" \
      org.opencontainers.image.licenses="MIT"
RUN apt-get update && apt-get install -y --no-install-recommends chromium ca-certificates tzdata \
    && rm -rf /var/lib/apt/lists/*
COPY --from=build /f95zone /usr/local/bin/f95zone
ENV F95_BASE_DIR=/data PORT=6069 CHROME_PATH=/usr/bin/chromium
VOLUME /data
EXPOSE 6069
HEALTHCHECK --interval=30s --timeout=5s --start-period=15s --retries=3 CMD ["f95zone", "-healthcheck"]
ENTRYPOINT ["f95zone"]
