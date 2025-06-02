FROM golang:1.22 AS builder WORKDIR /app COPY go.mod go.sum ./ RUN go mod download COPY . . RUN CGO_ENABLED=0 GOOS=linux go build -o main.
FROM alpine:latest WORKDIR /app COPY --from=builder /app/main . COPY credentials.json token.json ./ EXPOSE 8080 CMD ["./main"]
