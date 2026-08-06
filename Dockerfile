# syntax=docker/dockerfile:1
FROM node:24-alpine AS frontend

WORKDIR /src
COPY web/package.json web/package-lock.json ./web/
RUN npm ci --ignore-scripts --prefix web
COPY web ./web
RUN npm run --prefix web build:all

FROM golang:1.26-alpine AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
COPY --from=frontend /src/internal/app/adminui/dist ./internal/app/adminui/dist
COPY --from=frontend /src/internal/app/publicui/dist ./internal/app/publicui/dist
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/generals-server ./cmd/generals-server \
    && mkdir -p /out/data \
    && chmod 0700 /out/data \
    && chown 65532:65532 /out/data

FROM scratch
COPY --from=build /out/generals-server /generals-server
COPY --from=build --chown=65532:65532 /out/data /data
USER 65532:65532
EXPOSE 29900/tcp 27901/udp 8080/tcp 8081/tcp 8083/tcp 8443/tcp
ENTRYPOINT ["/generals-server"]
