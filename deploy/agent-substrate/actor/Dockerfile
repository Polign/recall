# The demo actor. Build from the repository root:
#
#   docker build -f deploy/agent-substrate/actor/Dockerfile -t localhost:5001/recall-demo-actor:dev .

FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build
ENV GOTOOLCHAIN=auto
WORKDIR /src
COPY go.mod go.sum ./
COPY deploy/agent-substrate/actor ./deploy/agent-substrate/actor
ARG TARGETOS TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w" -o /out/actor ./deploy/agent-substrate/actor

FROM gcr.io/distroless/static-debian12
COPY --from=build /out/actor /actor
EXPOSE 80
ENTRYPOINT ["/actor"]
