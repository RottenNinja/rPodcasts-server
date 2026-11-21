FROM golang:1.27-trixie AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# A release carries its number in a VERSION file; a build of unreleased source is "dev".
ARG VERSION
RUN CGO_ENABLED=1 go build -trimpath -ldflags "-s -w -X main.version=${VERSION:-$(cat VERSION 2>/dev/null || echo dev)}" -o /podcasts ./cmd/podcasts

# The image has no downloader engine; the server fetches it into /data/state from PODCASTS_ENGINE_URL on start.
FROM debian:trixie-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates ffmpeg python3 curl \
 && rm -rf /var/lib/apt/lists/* \
 && useradd -r -u 10001 -d /data podcasts \
 && mkdir -p /data/state && chown -R podcasts /data
COPY --from=build /podcasts /usr/local/bin/podcasts
USER podcasts
ENV PODCASTS_DB=/data/state/podcasts.db PODCASTS_ADDR=:8484
WORKDIR /data
EXPOSE 8484
HEALTHCHECK CMD curl -fsS http://127.0.0.1:8484/healthz || exit 1
ENTRYPOINT ["/usr/local/bin/podcasts"]
CMD ["serve"]
