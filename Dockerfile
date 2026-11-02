FROM golang:1.27.3-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o blackbox ./cmd/blackbox

# Lighthouse builds this stage before every deploy: a failure stops the
# deploy. Database tests skip here (no Postgres); scripts/test-db.sh runs them.
FROM builder AS test
RUN apk add --no-cache gcc musl-dev
RUN unformatted="$(gofmt -l .)" && [ -z "$unformatted" ] || { echo "These files need gofmt:"; echo "$unformatted"; exit 1; }
RUN go vet ./...
RUN CGO_ENABLED=1 go test -race -short -count=1 ./...

# Pinned on purpose; bump it deliberately (e.g. for security fixes).
FROM alpine:3.24
# /run/blackbox-proxy stays root's: the proxy (root) creates its socket there.
RUN apk add --no-cache ca-certificates \
 && mkdir -p /run/blackbox-proxy
COPY --from=builder /app/blackbox /blackbox
CMD ["/blackbox", "help"]
