FROM golang:1.25 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go test ./internal/clinical/ && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /glucoguard ./cmd/server

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /glucoguard /glucoguard
EXPOSE 8080
USER nonroot
ENTRYPOINT ["/glucoguard"]
