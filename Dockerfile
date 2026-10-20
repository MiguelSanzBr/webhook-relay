FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags=-s -o /out/relay ./cmd/relay

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/relay /relay
ENTRYPOINT ["/relay"]
