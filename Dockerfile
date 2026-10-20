# Build a static Linux binary, then copy it into a minimal image with no shell.
FROM golang:1.23-bookworm AS build

WORKDIR /src

COPY go.mod ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath \
    -ldflags="-s -w" \
    -o /out/kbloom \
    ./cmd/kbloom

FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/kbloom /kbloom

USER nonroot:nonroot

EXPOSE 6380

# Logs stay on stderr so Docker and Kubernetes collect them.
ENV KBLOOM_ADDR=:6380

ENTRYPOINT ["/kbloom"]

HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
    CMD ["/kbloom", "health"]
