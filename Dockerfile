# Ref: oga9-engineering/sections/09-deployment-editions.md — §14.01
# versioned image `oga9:{version}`; api+worker+scheduler ship in one binary.
FROM golang:1.26-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/oga9 ./cmd/api

FROM debian:bookworm-slim
# No postgres client baked in — bookworm ships pg15, which can't pg_dump a
# pg18 server; backup/restore run inside the postgres container instead
# (docker compose exec postgres pg_dump ...) or via scripts/ on a host
# with matching tooling.
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates \
 && rm -rf /var/lib/apt/lists/* \
 && groupadd -r oga9 && useradd -r -g oga9 -u 10001 oga9 \
 && mkdir -p /data/uploads && chown -R oga9:oga9 /data
COPY --from=build /out/oga9 /usr/local/bin/oga9
COPY db/migrations /opt/oga9/migrations
COPY scripts /opt/oga9/scripts
ENV OGA9_UPLOADS_DIR=/data/uploads
VOLUME ["/data"]
EXPOSE 8080
# non-root: the api needs no privileges — keeps file: credential_refs and
# any future fs surface inside its own data dir at worst.
USER oga9
ENTRYPOINT ["oga9"]
