FROM golang:1.24-alpine AS build
WORKDIR /src
ARG VERSION=v0.21.0
COPY . .
RUN VERSION_NO_V="${VERSION#v}" && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION_NO_V} -X github.com/ali-shortcuts/nexaroute/internal/httpapi.gatewayVersion=${VERSION_NO_V}" -o /out/nexaroute ./cmd/gateway \
    && mkdir -m 0700 -p /out/config \
    && cp configs/config.example.json /out/config/config.json \
    && chmod 0600 /out/config/config.json

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/nexaroute /nexaroute
COPY --from=build --chown=65532:65532 /out/config /config
ENV NEXAROUTE_LISTEN=0.0.0.0:8080 NEXAROUTE_ADMIN_BIND_LOCAL_ONLY=false
EXPOSE 8080
ENTRYPOINT ["/nexaroute","-no-browser","-config","/config/config.json"]
