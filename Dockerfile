# One image holds every backend binary; Compose picks the command.
# Builds for linux/arm64 (Oracle free VM) and linux/amd64.

FROM golang:1.27-alpine AS build
WORKDIR /src
ENV CGO_ENABLED=0
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN for cmd in api worker migrate seed sim-notify admin; do \
      go build -trimpath -ldflags="-s -w" -o /out/$cmd ./cmd/$cmd; \
    done

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/ /usr/local/bin/
ARG VERSION=dev
ENV APP_VERSION=$VERSION
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT []
CMD ["/usr/local/bin/api"]
