FROM golang:1.26-alpine AS build

WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 go build -ldflags "-s -w" -o /out/companion ./cmd/companion

FROM alpine:latest
COPY --from=build /out/companion /companion
ENTRYPOINT ["/companion"]
