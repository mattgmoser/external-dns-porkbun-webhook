## Multi-stage, multi-arch.
## Builds for amd64, arm64, arm/v7. Final image is distroless static (~3 MB + binary).

# Keep the Go version a literal tag: Dependabot cannot parse an ARG-interpolated
# FROM, so a variable here silently hides Go security releases from it. It must
# match the go.mod toolchain line, which CI builds and tests with
# (scripts/verify-go-version.sh enforces this).
FROM --platform=$BUILDPLATFORM golang:1.27.2-alpine AS build
WORKDIR /src

# Cache dependencies
COPY go.mod go.sum ./
RUN go mod download

# Build
COPY . .
ARG TARGETOS TARGETARCH TARGETVARIANT
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} GOARM=${TARGETVARIANT#v} \
    go build -trimpath -ldflags="-s -w -X main.Version=${VERSION}" \
    -o /out/external-dns-porkbun-webhook ./

## Runtime: distroless static (no shell, no package manager).
# Pinned by index digest so a local pre-tag image scan and the release build use
# the same base. Dependabot proposes digest refreshes for this tag.
FROM gcr.io/distroless/static-debian13:nonroot@sha256:e2e927ec666bae08560abb3c55d0659eceabb657f56b6782ab500a9fc7f555e3
COPY --from=build /out/external-dns-porkbun-webhook /usr/local/bin/external-dns-porkbun-webhook
USER 65532:65532
EXPOSE 8888 8080
ENTRYPOINT ["/usr/local/bin/external-dns-porkbun-webhook"]
