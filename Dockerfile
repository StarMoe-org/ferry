# syntax=docker/dockerfile:1

FROM node:22-alpine AS web
WORKDIR /web
COPY web/package.json web/package-lock.json ./
RUN npm ci --no-audit --no-fund
COPY web/ ./
RUN npm run build

FROM golang:1.25-alpine AS build
ARG TARGETOS=linux
ARG TARGETARCH=amd64
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ ./cmd/
COPY internal/ ./internal/
COPY web/embed.go ./web/embed.go
COPY --from=web /web/dist ./web/dist
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/ferry ./cmd/ferry

FROM alpine:3.21 AS sops
ARG TARGETARCH=amd64
ARG SOPS_VERSION=3.13.3
ARG SOPS_SHA256_AMD64=e5bec3346a873ae91d871550f3e698c1aad962aff462a080e40f25fde17fef6b
ARG SOPS_SHA256_ARM64=53b0abacd38ef1b12a66d6c100956691b9cefce018d91f81e73ddf7438b94d77
RUN apk add --no-cache ca-certificates curl && \
    case "${TARGETARCH}" in \
      amd64) SOPS_SHA256="${SOPS_SHA256_AMD64}" ;; \
      arm64) SOPS_SHA256="${SOPS_SHA256_ARM64}" ;; \
      *) echo "unsupported TARGETARCH: ${TARGETARCH}" >&2; exit 1 ;; \
    esac && \
    curl -fsSL "https://github.com/getsops/sops/releases/download/v${SOPS_VERSION}/sops-v${SOPS_VERSION}.linux.${TARGETARCH}" \
      -o /usr/local/bin/sops && \
    echo "${SOPS_SHA256}  /usr/local/bin/sops" | sha256sum -c - && \
    chmod 0755 /usr/local/bin/sops

FROM alpine:3.21
RUN apk add --no-cache ca-certificates git && \
    mkdir -p /work && chown 65532:65532 /work
COPY --from=sops /usr/local/bin/sops /usr/local/bin/sops
COPY --from=build /out/ferry /usr/local/bin/ferry
USER 65532:65532
WORKDIR /work
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/ferry"]
CMD ["-config", "/etc/ferry/ferry.yaml"]
