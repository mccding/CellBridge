FROM golang:1.25-bookworm AS build

WORKDIR /src
COPY gateway/go.mod gateway/go.sum ./
ARG GOPROXY=https://proxy.golang.org,direct
RUN GOPROXY=${GOPROXY} go mod download
COPY gateway/ ./
ARG TARGETARCH=amd64
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags='-s -w' -o /out/cellbridge-gateway ./cmd/cellbridge-gateway
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags='-s -w' -o /out/cellbridge-admin ./cmd/cellbridge-admin
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags='-s -w' -o /out/cellbridge-probe ./cmd/cellbridge-probe

FROM debian:bookworm-slim

RUN apt-get update && apt-get install -y --no-install-recommends alsa-utils ffmpeg ca-certificates wget \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd --system cellbridge && useradd --system --gid cellbridge --home-dir /var/lib/cellbridge cellbridge
COPY --from=build /out/cellbridge-gateway /usr/local/bin/cellbridge-gateway
COPY --from=build /out/cellbridge-admin /usr/local/bin/cellbridge-admin
COPY --from=build /out/cellbridge-probe /usr/local/bin/cellbridge-probe
COPY infra/config.example.yaml /etc/cellbridge/config.example.yaml
RUN mkdir -p /var/lib/cellbridge && chown -R cellbridge:cellbridge /var/lib/cellbridge

USER cellbridge
VOLUME ["/var/lib/cellbridge"]
EXPOSE 8787
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s CMD ["wget", "-qO-", "http://127.0.0.1:8787/api/v1/health"]
ENTRYPOINT ["/usr/local/bin/cellbridge-gateway"]
