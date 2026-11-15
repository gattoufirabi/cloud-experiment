# One static binary, no shell, no package manager in the final image.
FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/platform ./cmd/platform

FROM scratch
COPY --from=build /out/platform /platform
USER 65532:65532
ENTRYPOINT ["/platform"]
