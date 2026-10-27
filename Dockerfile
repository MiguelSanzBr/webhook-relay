FROM golang:1.27.3-alpine@sha256:f2c872bcee949b7be9c490efd02b48a8a2fbf947cdf533f1c58525ec9d8170fd AS build
WORKDIR /src
COPY . .
RUN --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags=-s -o /out/relay ./cmd/relay

FROM gcr.io/distroless/static-debian13:nonroot@sha256:18f4cc26c9ffa7f20a359298be7deadac5c607c8e03a02b8119f1a5fc2f82c99
COPY --from=build /out/relay /relay
EXPOSE 8080
# distroless não tem shell nem curl: o próprio binário bate no /healthz
HEALTHCHECK --interval=10s --timeout=3s --start-period=5s CMD ["/relay", "-health"]
ENTRYPOINT ["/relay"]
