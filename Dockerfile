# Match the Debian 12 distroless runtime's glibc when linking the CGO binary.
FROM golang:1.26-bookworm AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=1 go build -trimpath -ldflags "-s -w" -o /out/notinv ./cmd/notinv
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/healthcheck ./cmd/healthcheck

FROM gcr.io/distroless/cc-debian12:nonroot

WORKDIR /app
COPY --from=build /out/notinv /notinv
COPY --from=build /out/healthcheck /healthcheck
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 CMD ["/healthcheck"]
ENTRYPOINT ["/notinv"]
