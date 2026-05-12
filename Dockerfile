FROM golang:1.26 AS builder

WORKDIR /src
COPY . .

ENV PATH="/usr/local/go/bin:${PATH}"

ARG COMPANION_VERSION=0.1.0-dev
ARG COMPANION_GIT_SHA=dev
ARG COMPANION_BUILD_DATE=unknown

RUN gofmt -w $(find . -name '*.go')
RUN CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7,hardfloat \
    go build -buildvcs=false -trimpath \
    -ldflags="-s -w -X bticino-go-companion/internal/config.BuildVersion=${COMPANION_VERSION} -X bticino-go-companion/internal/config.BuildGitSHA=${COMPANION_GIT_SHA} -X bticino-go-companion/internal/config.BuildDate=${COMPANION_BUILD_DATE}" \
    -o /out/companion ./cmd/companion

FROM scratch AS artifact
COPY --from=builder /out/companion /companion
