FROM node:24-bookworm-slim AS web
WORKDIR /build/web
COPY apps/web/package*.json ./
RUN npm ci
COPY apps/web/ ./
RUN npm run build

FROM golang:1.26-bookworm AS go-build
WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ ./cmd/
COPY internal/ ./internal/
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/mp-api ./cmd/mp-api
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/mp-worker ./cmd/mp-worker
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o /out/mp-agent-linux-amd64 ./cmd/mp-agent

FROM debian:bookworm-slim AS runtime
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates && rm -rf /var/lib/apt/lists/* \
    && groupadd --gid 10001 mp && useradd --uid 10001 --gid 10001 --no-create-home mp \
    && mkdir -p /var/lib/mp /opt/mp/web && chown -R 10001:10001 /var/lib/mp
COPY --from=go-build /out/mp-api /out/mp-worker /usr/local/bin/
COPY --from=go-build /out/mp-agent-linux-amd64 /opt/mp/mp-agent-linux-amd64
COPY --from=web /build/web/dist/ /opt/mp/web/
ENV MP_DATA_DIR=/var/lib/mp MP_WEB_DIR=/opt/mp/web MP_AGENT_BINARY=/opt/mp/mp-agent-linux-amd64
USER 10001:10001
WORKDIR /opt/mp
EXPOSE 8080 8443
ENTRYPOINT ["/usr/local/bin/mp-api"]
