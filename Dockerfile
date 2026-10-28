# syntax=docker/dockerfile:1

# Cross-compile on the build host's native platform; no emulation needed.
FROM --platform=$BUILDPLATFORM golang:1.27.3 AS build

ARG TARGETOS
ARG TARGETARCH

WORKDIR /src

# Download modules first so they stay cached until go.mod/go.sum change.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY cmd ./cmd
COPY internal ./internal

RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags='-s -w' -o /out/nx-remote-cache ./cmd/nx-remote-cache

FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/nx-remote-cache /nx-remote-cache

EXPOSE 3000 9090
# distroless `nonroot` (65532), numeric so Kubernetes runAsNonRoot can verify it.
USER 65532:65532
ENTRYPOINT ["/nx-remote-cache"]
