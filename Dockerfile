FROM golang:1.27.4-alpine@sha256:70bd7ddea8b6cbe5d6943cde279d6b67feb10cc924fb9100d1d8757c1575b280 AS build
WORKDIR /src
COPY . .
RUN --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags=-s -o /out/relay ./cmd/relay

FROM gcr.io/distroless/static-debian13:nonroot@sha256:516757106de35205efb70b91ba03f6f1298424a0678c603255cf2852a7000087
COPY --from=build /out/relay /relay
EXPOSE 8080
# distroless não tem shell nem curl: o próprio binário bate no /healthz
HEALTHCHECK --interval=10s --timeout=3s --start-period=5s CMD ["/relay", "-health"]
ENTRYPOINT ["/relay"]
