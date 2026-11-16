# Guvna — multi-stage build to a distroless static image.
# Result: ~19MB, no shell, no package manager, runs as nonroot (UID 65532).

FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/guvna ./cmd/guvna \
 && mkdir -p /data && chown 65532:65532 /data

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/guvna /guvna
COPY --chown=65532:65532 --from=build /data /data
COPY config.yaml /config.yaml
EXPOSE 20128
USER nonroot
# Liveness probe: the binary itself GETs /healthz (no shell/curl in the
# image). Compose also declares this healthcheck; the image-level one
# covers plain `docker run` users (see docs/OPERATIONS.md).
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
  CMD ["/guvna", "-healthcheck", "-config", "/config.yaml"]
ENTRYPOINT ["/guvna", "-config", "/config.yaml", "-data", "/data"]
