# ============================================================
# Stage 1: Build the picoclaw binary
# ============================================================
FROM golang:1.26.10-alpine AS builder

RUN apk add --no-cache git make openssh-client

WORKDIR /src
ENV GOPRIVATE=github.com/adapta-school/hulk-sdk-go

# Cache dependencies
COPY go.mod go.sum ./
RUN --mount=type=secret,id=hulk_sdk_key,target=/run/hulk-sdk/key,required=true \
    --mount=type=secret,id=hulk_sdk_hosts,target=/run/hulk-sdk/known_hosts,required=true \
    --mount=type=secret,id=hulk_sdk_gitconfig,target=/run/hulk-sdk/gitconfig,required=true \
    HULK_SDK_GO_AUTH_DIR=/run/hulk-sdk GIT_CONFIG_GLOBAL=/run/hulk-sdk/gitconfig go mod download

# Copy source and build
COPY . .
RUN --mount=type=secret,id=hulk_sdk_key,target=/run/hulk-sdk/key,required=true \
    --mount=type=secret,id=hulk_sdk_hosts,target=/run/hulk-sdk/known_hosts,required=true \
    --mount=type=secret,id=hulk_sdk_gitconfig,target=/run/hulk-sdk/gitconfig,required=true \
    HULK_SDK_GO_AUTH_DIR=/run/hulk-sdk GIT_CONFIG_GLOBAL=/run/hulk-sdk/gitconfig make build

# ============================================================
# Stage 2: Minimal runtime image
# ============================================================
FROM alpine:3.23

RUN apk add --no-cache ca-certificates tzdata curl

# Health check
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
  CMD wget -q --spider http://localhost:18790/health || exit 1

# Copy binary and first-run entrypoint (same as release image).
COPY --from=builder /src/build/picoclaw /usr/local/bin/picoclaw
COPY docker/entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

ENTRYPOINT ["/entrypoint.sh"]
