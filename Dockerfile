FROM golang:1.26-alpine AS build
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/nexaroute ./cmd/gateway \
    && mkdir -m 0700 -p /out/config \
    && cp configs/config.example.json /out/config/config.json \
    && chmod 0600 /out/config/config.json

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/nexaroute /nexaroute
COPY --from=build --chown=65532:65532 /out/config /config
ENV NEXAROUTE_LISTEN=0.0.0.0:8080 NEXAROUTE_ADMIN_BIND_LOCAL_ONLY=false
EXPOSE 8080
ENTRYPOINT ["/nexaroute","-config","/config/config.json"]
