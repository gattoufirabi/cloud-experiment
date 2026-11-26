FROM golang:1.23-alpine AS build
WORKDIR /src
COPY main.go .
RUN go mod init loadtest && CGO_ENABLED=0 go build -ldflags=-s -o /app .

FROM scratch
COPY --from=build /app /app
USER 65534
EXPOSE 8080
ENTRYPOINT ["/app"]
