# ---- build stage ----
# Pinned to the *builder's* platform on purpose: the Go toolchain cross-compiles
# in seconds, where QEMU emulating an arm64 toolchain natively takes minutes.
FROM --platform=$BUILDPLATFORM golang:1.26-bookworm AS build
WORKDIR /src
ARG VERSION=dev
ARG COMMIT=none
# Supplied by BuildKit, once per target platform. Empty under the classic
# builder, which Go reads as "unset" and falls back to the host arch — the
# single-platform behaviour this had before.
ARG TARGETARCH

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} go build -trimpath \
    -ldflags "-s -w \
      -X github.com/koungkub/tehran/internal/version.Version=${VERSION} \
      -X github.com/koungkub/tehran/internal/version.GitCommit=${COMMIT} \
      -X github.com/koungkub/tehran/internal/version.BuildDate=$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
    -o /out/tehran ./cmd/tehran

# ---- runtime stage ----
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/tehran /tehran
USER nonroot:nonroot
EXPOSE 8080 9090
ENTRYPOINT ["/tehran"]
CMD ["api"]
