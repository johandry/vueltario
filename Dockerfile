FROM golang:1.27-alpine AS builder
WORKDIR /src
RUN apk add --no-cache git ca-certificates
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/vueltario ./cmd/vueltario

FROM alpine:latest
RUN apk add --no-cache ca-certificates ffmpeg tzdata && \
    addgroup -S vueltario && adduser -S -G vueltario vueltario && \
    mkdir -p /var/lib/vueltario/storage && chown -R vueltario:vueltario /var/lib/vueltario
COPY --from=builder /out/vueltario /usr/local/bin/vueltario
USER vueltario
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/vueltario"]
CMD ["serve"]
