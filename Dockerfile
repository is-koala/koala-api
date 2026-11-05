# Multi-stage build (plan §3.11, ADR-0004). Both base images are pinned by
# tag and digest; the golang tag follows the toolchain line in go.mod.
# Images are never built on the shared Mac. CI builds, smoke-tests and
# publishes them with scripts/image.sh (ADR-0023); by hand:
#   docker build --build-arg VERSION=$(git describe --tags --always) -t koala-api .

FROM golang:1.27.3-trixie@sha256:c65ecfe613b539ac74c62a4c3b2c27e8f8a6076384c8f27e8a3c0f3be17cc385 AS build

WORKDIR /src
ENV CGO_ENABLED=0

COPY go.mod go.sum ./
RUN go mod download && go mod verify

COPY . .
ARG VERSION=dev
RUN go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/koala ./cmd/koala

FROM gcr.io/distroless/static-debian13:nonroot@sha256:25ac8f0c3b2334a9eb05452266bd4697864a8c8ef0ef71e5e6480d7f92868329

COPY --from=build /out/koala /koala

USER nonroot:nonroot
EXPOSE 8080

# The binary probes its own /livez; the image has no shell, curl or wget.
# Liveness, not readiness: a database outage must not get the container
# restarted.
HEALTHCHECK --interval=10s --timeout=5s --start-period=10s --retries=3 CMD ["/koala", "healthcheck"]

ENTRYPOINT ["/koala"]
CMD ["serve"]
