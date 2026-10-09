# syntax=docker/dockerfile:1

# Build on the host's architecture and cross-compile, so multi-arch builds
# don't run the Go toolchain under emulation.
FROM --platform=$BUILDPLATFORM golang:1.26 AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/siftstr ./cmd/siftstr
# Pre-create /data owned by the runtime user so a fresh named volume inherits it.
RUN mkdir /out/data

# distroless/static ships CA certificates and tzdata, and has no shell.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/siftstr /siftstr
COPY --from=build --chown=nonroot:nonroot /out/data /data

ENV SIFTSTR_LISTEN=:8080 \
    SIFTSTR_DATA_DIR=/data
VOLUME /data
EXPOSE 8080
USER nonroot:nonroot

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD ["/siftstr", "healthcheck"]

ENTRYPOINT ["/siftstr"]
CMD ["serve"]
