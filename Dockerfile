#FROM m.daocloud.io/docker.io/library/golang:1.26-alpine AS builder
FROM golang:1.26-alpine AS builder

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o server .

#FROM m.daocloud.io/docker.io/library/alpine:3.18
FROM alpine:3.18

RUN apk --no-cache add ca-certificates
WORKDIR /app
COPY --from=builder /app/server .
RUN mkdir -p /var/lib/docker-mirror
EXPOSE 5000
CMD ["./server"]
