# One static binary, no shell, no package manager in the final image.
FROM golang:1.23-alpine AS build
RUN apk add --no-cache ca-certificates
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/platform ./cmd/platform

FROM scratch
COPY --from=build /out/platform /platform
# Public CA roots: outgoing HTTPS (webhooks, identity providers, the node agent) must verify certificates.
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
USER 65532:65532
ENTRYPOINT ["/platform"]
