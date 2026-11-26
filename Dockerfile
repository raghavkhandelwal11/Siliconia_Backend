# One image holds every backend binary; Compose picks the command.
# Builds for linux/arm64 (Oracle free VM) and linux/amd64.

# Cross-compiles on the build machine's own CPU (no emulation) for each target.
FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
ENV CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN for cmd in api worker migrate seed sim-notify sim-kyc sim-pay sim-ship sim-payout admin demo; do \
      go build -trimpath -ldflags="-s -w" -o /out/$cmd ./cmd/$cmd; \
    done && mkdir -p /data/media

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/ /usr/local/bin/
# Product photos on the lite server (STORAGE_DRIVER=disk); a named volume
# mounted here starts with this ownership, so the non-root user can write.
COPY --from=build --chown=65532:65532 /data /data
ARG VERSION=dev
ENV APP_VERSION=$VERSION
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT []
CMD ["/usr/local/bin/api"]
