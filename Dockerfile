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
 && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/oga9 /usr/local/bin/oga9
COPY db/migrations /opt/oga9/migrations
COPY scripts /opt/oga9/scripts
ENV OGA9_UPLOADS_DIR=/data/uploads
VOLUME ["/data"]
EXPOSE 8080
ENTRYPOINT ["oga9"]
