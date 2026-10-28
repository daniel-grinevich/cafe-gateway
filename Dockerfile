# syntax=docker/dockerfile:1
# One image for both services; BARISTA_SERVICE=barista|grinder picks which one `serve` runs.

FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
ARG TARGETOS TARGETARCH
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/ ./cmd/cafe-gateway ./cmd/mock-openrouter

# Local development only (compose): a fake OpenRouter.
FROM gcr.io/distroless/static-debian12:nonroot AS mock-openrouter
COPY --from=build /out/mock-openrouter /usr/local/bin/mock-openrouter
EXPOSE 8090
ENTRYPOINT ["mock-openrouter"]

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/cafe-gateway /usr/local/bin/cafe-gateway
EXPOSE 8000
HEALTHCHECK --interval=15s --timeout=5s --start-period=5s CMD ["cafe-gateway", "healthcheck"]
ENTRYPOINT ["cafe-gateway"]
CMD ["serve"]
