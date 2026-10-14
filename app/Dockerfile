FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/redis-app .

FROM alpine:3.22
COPY --from=build /out/redis-app /usr/local/bin/redis-app
ENTRYPOINT ["/usr/local/bin/redis-app"]
