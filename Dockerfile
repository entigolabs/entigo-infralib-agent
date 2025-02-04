FROM --platform=$BUILDPLATFORM golang:1.23-alpine AS build
WORKDIR /go/ei-agent
COPY go.* ./
RUN go mod download
COPY . .
ARG GITHUB_SHA=main
ARG VERSION=latest
ARG TARGETARCH
ARG TARGETOS

RUN --mount=target=. \
    --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg \
    GOOS="$TARGETOS" GOARCH="$TARGETARCH" go build -ldflags \
     "-X github.com/entigolabs/entigo-infralib-agent/common.version=${VERSION} \
      -X github.com/entigolabs/entigo-infralib-agent/common.buildDate=$(date -u +'%Y-%m-%dT%H:%M:%SZ') \
      -X github.com/entigolabs/entigo-infralib-agent/common.gitCommit=${GITHUB_SHA} \
               -extldflags -static" -o /out/ei-agent main.go

FROM alpine:3
WORKDIR /etc/ei-agent
COPY --from=build /out/ei-agent /usr/bin/
CMD ["ei-agent", "run"]
