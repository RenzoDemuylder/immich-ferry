# syntax=docker/dockerfile:1
FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/immich-ferry ./cmd/immich-ferry \
 && mkdir -p /out/data

FROM gcr.io/distroless/static-debian12:nonroot
LABEL org.opencontainers.image.title="immich-ferry" \
      org.opencontainers.image.description="Bidirectional album sync between two Immich instances via the REST API"
COPY --from=build /out/immich-ferry /immich-ferry
COPY --from=build --chown=65532:65532 /out/data /data
# No ENV for SYNC_STATE_DIR / HTTP_ADDR: env beats the YAML config file, so setting them here would
# silently override stateDir/httpAddr from a mounted config. The code defaults are /data and :8080,
# and the healthcheck follows HTTP_ADDR, then SYNC_CONFIG_FILE's httpAddr, then :8080.
VOLUME ["/data"]
EXPOSE 8080
USER 65532:65532
HEALTHCHECK --interval=30s --timeout=5s --start-period=30s --retries=3 \
  CMD ["/immich-ferry","healthcheck"]
ENTRYPOINT ["/immich-ferry"]
CMD ["run"]
