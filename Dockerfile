# Multi-stage build (plan §3.11, ADR-0004). Both base images are pinned by
# tag and digest; bump the golang tag together with the go directive in
# go.mod. Build with:
#   docker build --build-arg VERSION=$(git describe --tags --always) -t koala-api .

FROM golang:1.25.5-trixie@sha256:1763926ff08117bdf4c76182c02fecbcfc3086771169b2a03574273dde17fcee AS build

WORKDIR /src
ENV CGO_ENABLED=0 GOTOOLCHAIN=local

COPY go.mod go.sum ./
RUN go mod download && go mod verify

COPY . .
ARG VERSION=dev
RUN go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/koala ./cmd/koala

FROM gcr.io/distroless/static-debian13:nonroot@sha256:25ac8f0c3b2334a9eb05452266bd4697864a8c8ef0ef71e5e6480d7f92868329

COPY --from=build /out/koala /koala

USER nonroot:nonroot
EXPOSE 8080

# The binary probes itself; the image has no shell, curl or wget.
HEALTHCHECK --interval=10s --timeout=5s --start-period=10s --retries=3 CMD ["/koala", "healthcheck"]

ENTRYPOINT ["/koala"]
CMD ["serve"]
