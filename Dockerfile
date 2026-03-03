FROM golang:1.24-alpine AS builder

ARG SERVICE

WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /app/service ./cmd/${SERVICE}

FROM alpine:3.19
RUN apk add --no-cache ca-certificates tzdata
WORKDIR /app
COPY --from=builder /app/service /app/service
COPY configs/ /app/configs/

ENTRYPOINT ["/app/service"]
