FROM golang:1.25-alpine AS builder
WORKDIR /app
COPY go.mod ./
COPY main.go ./
RUN go build -o metadata-api main.go

FROM alpine:3.19
RUN apk add --no-cache ca-certificates
WORKDIR /app
COPY --from=builder /app/metadata-api .

# /data is where metadata.json is persisted. Mount a volume here.
VOLUME ["/data"]
ENV DATA_FILE=/mnt/data/verseye/api-config/metadata.json
ENV PORT=8080

EXPOSE 8080
CMD ["./metadata-api"]