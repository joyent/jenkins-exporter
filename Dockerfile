# Build stage
FROM public.ecr.aws/docker/library/golang:1.21-alpine AS builder

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o jenkins-exporter .

# Runtime stage
FROM alpine:latest

RUN apk --no-cache add ca-certificates
WORKDIR /app

COPY --from=builder /app/jenkins-exporter .

EXPOSE 9506

CMD ["./jenkins-exporter"]