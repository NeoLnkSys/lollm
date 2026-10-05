# LoLLM Synapse — multi-stage build (static binary, minimal runtime)
FROM golang:1.27-alpine AS build
ARG VERSION=0.1.0
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# modernc.org/libc is enormous; build serially with a GC cap (see Makefile).
RUN CGO_ENABLED=0 GOGC=50 GOMEMLIMIT=1500MiB \
    go build -p 1 -trimpath -ldflags "-s -w \
      -X github.com/lollm/lollm/internal/cli.Version=${VERSION}" \
      -o /out/lollm ./cmd/lollm

FROM alpine:3.20
RUN adduser -D -u 10001 lollm && mkdir -p /data && chown lollm /data
COPY --from=build /out/lollm /usr/local/bin/lollm
USER lollm
VOLUME /data
ENV HOST=0.0.0.0 API_PORT=20999 DASHBOARD_PORT=21000 DB_PATH=/data/lollm.db
EXPOSE 20999 21000
ENTRYPOINT ["lollm"]
CMD ["serve", "--host", "0.0.0.0", "--api-port", "20999", "--dashboard-port", "21000"]
