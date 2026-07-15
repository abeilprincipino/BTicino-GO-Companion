FROM golang:1.26-alpine AS test

WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
RUN go test ./...

FROM test AS build
ARG VERSION=dev
ARG GIT_SHA=-
ARG RELEASE_REPO=
RUN CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 go build -ldflags "-s -w -X bticino-go-companion/internal/system.BuildVersion=${VERSION} -X bticino-go-companion/internal/system.BuildGitSHA=${GIT_SHA} -X bticino-go-companion/internal/system.BuildReleaseRepo=${RELEASE_REPO}" -o /out/companion ./cmd/companion

FROM alpine:latest
COPY --from=build /out/companion /companion
ENTRYPOINT ["/companion"]
