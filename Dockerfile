# Build stage
FROM golang:1.26-alpine AS builder
WORKDIR /workspace

# Copy module files and download dependencies first (cache layer).
COPY go.mod go.sum ./
RUN go mod download

# Copy source and build a statically linked binary.
COPY api/       api/
COPY cmd/       cmd/
COPY internal/  internal/

RUN CGO_ENABLED=0 GOOS=linux go build -a -o manager ./cmd/main.go

# Runtime stage — distroless for minimal attack surface.
FROM gcr.io/distroless/static:nonroot
WORKDIR /

COPY --from=builder /workspace/manager .

# Run as non-root user (65532 = nonroot in distroless).
USER 65532:65532

ENTRYPOINT ["/manager"]
