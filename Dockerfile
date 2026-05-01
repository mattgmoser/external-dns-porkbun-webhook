## Multi-stage, multi-arch.
## Builds for amd64, arm64, arm/v7. Final image is distroless static (~3 MB + binary).

ARG GO_VERSION=1.23
FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine AS build
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
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/external-dns-porkbun-webhook /usr/local/bin/external-dns-porkbun-webhook
USER 65532:65532
EXPOSE 8888 8080
ENTRYPOINT ["/usr/local/bin/external-dns-porkbun-webhook"]
