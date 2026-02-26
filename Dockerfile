# syntax=docker/dockerfile:1

# ── Build stage ───────────────────────────────────────────────────────────────
FROM golang:1.24-alpine AS builder

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux \
    go build -trimpath -o /containerd-clone-snapshotter ./cmd/containerd-clone-snapshotter

# ── Final stage ───────────────────────────────────────────────────────────────
FROM scratch

COPY --from=builder /containerd-clone-snapshotter /containerd-clone-snapshotter

ENTRYPOINT ["/containerd-clone-snapshotter"]
