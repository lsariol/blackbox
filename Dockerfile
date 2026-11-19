FROM golang:1.27.4-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o blackbox ./cmd/blackbox

# Built before every deploy: a failure stops it. Database tests skip here, with no Postgres.
FROM builder AS test
RUN apk add --no-cache gcc musl-dev
RUN unformatted="$(gofmt -l .)" && [ -z "$unformatted" ] || { echo "These files need gofmt:"; echo "$unformatted"; exit 1; }
RUN go vet ./...
RUN CGO_ENABLED=1 go test -race -short -count=1 ./...

# Pinned on purpose; bump it deliberately (e.g. for security fixes).
FROM alpine:3.24
# The proxy creates its socket in /run/blackbox-proxy, as root.
RUN apk add --no-cache ca-certificates \
 && mkdir -p /run/blackbox-proxy
COPY --from=builder /app/blackbox /blackbox
USER 10001:10001
CMD ["/blackbox", "help"]
