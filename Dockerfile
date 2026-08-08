# Build stage
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/gateway ./cmd/gateway

# Runtime stage
FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata \
 && addgroup -S gateway \
 && adduser -S -G gateway gateway
WORKDIR /app
COPY --from=build /out/gateway /app/gateway
USER gateway
EXPOSE 8080
ENTRYPOINT ["/app/gateway"]
