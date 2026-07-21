# ---- build stage ----
FROM golang:1.26-bookworm AS build
WORKDIR /src
ARG VERSION=dev
ARG COMMIT=none

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath \
    -ldflags "-s -w \
      -X github.com/koungkub/tehran/internal/platform/version.Version=${VERSION} \
      -X github.com/koungkub/tehran/internal/platform/version.GitCommit=${COMMIT} \
      -X github.com/koungkub/tehran/internal/platform/version.BuildDate=$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
    -o /out/tehran ./cmd/tehran

# ---- runtime stage ----
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/tehran /tehran
USER nonroot:nonroot
EXPOSE 8080 9090
ENTRYPOINT ["/tehran"]
CMD ["api"]
