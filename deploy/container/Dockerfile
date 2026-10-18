# ghcr.io/polign/recall: the recall binary on a distroless base.
#
# One image, two jobs. As the server it runs `recall serve` (the default
# entrypoint). Inside a workload it runs `recall connect`, the stdio MCP
# bridge to a server, which is what the agent-sandbox manifests use.
#
# Recall is open source, so the image builds from this repository's source.
# Build from the repository root:
#
#   docker build -f deploy/container/Dockerfile -t ghcr.io/polign/recall:dev .

FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build
# The image pins GOTOOLCHAIN=local; auto lets a newer go.mod requirement
# fetch its toolchain instead of failing the build.
ENV GOTOOLCHAIN=auto
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS TARGETARCH VERSION=dev
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/recall ./cmd/recall

FROM gcr.io/distroless/static-debian12:nonroot
ARG VERSION=dev
LABEL org.opencontainers.image.title="Recall" \
      org.opencontainers.image.description="Agent memory: recall serve and recall connect" \
      org.opencontainers.image.source="https://github.com/Polign/recall" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.version="${VERSION}"
COPY --from=build --chown=65532:65532 /out/recall /usr/local/bin/recall
COPY LICENSE /LICENSE
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/recall"]
CMD ["serve"]
