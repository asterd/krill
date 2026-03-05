FROM golang:1.22-alpine AS builder
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /krill ./cmd/krill/

FROM gcr.io/distroless/static:nonroot
COPY --from=builder /krill /krill
COPY krill.yaml /krill.yaml
EXPOSE 8080 8081
ENTRYPOINT ["/krill", "-config", "/krill.yaml"]
