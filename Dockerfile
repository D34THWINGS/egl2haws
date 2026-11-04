# syntax=docker/dockerfile:1
# Multi-arch without emulation: the Go stage runs on the build machine and
# cross-compiles; the final stage only copies the binary.
FROM --platform=$BUILDPLATFORM golang:1.24-alpine AS build
ARG TARGETOS TARGETARCH
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/egl2haws ./cmd/egl2haws

FROM gcr.io/distroless/static-debian12:nonroot
LABEL org.opencontainers.image.source="https://github.com/D34THWINGS/egl2haws" \
      org.opencontainers.image.description="Imports Eau du Grand Lyon water consumption into Home Assistant's Energy dashboard" \
      org.opencontainers.image.licenses="MIT"
COPY --from=build /out/egl2haws /egl2haws
# Numeric so Kubernetes can enforce runAsNonRoot (it can't check a user name).
# 65532 is distroless's nonroot user.
USER 65532:65532
ENTRYPOINT ["/egl2haws"]
CMD ["run"]
