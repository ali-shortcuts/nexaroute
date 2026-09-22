FROM golang:1.23-alpine AS build
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/gateway ./cmd/gateway
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/gateway /gateway
COPY configs/config.example.json /config/config.json
ENV ULG_LISTEN=0.0.0.0:8080 ULG_ADMIN_BIND_LOCAL_ONLY=false
EXPOSE 8080
ENTRYPOINT ["/gateway","-config","/config/config.json"]
