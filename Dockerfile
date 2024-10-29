FROM --platform=${BUILDPLATFORM:-linux/amd64} golang:1.23-alpine AS build
WORKDIR /go/ei-agent
RUN apk add build-base
COPY go.* ./
RUN go mod download
COPY . ./
ARG GITHUB_SHA=main VERSION=latest TARGETPLATFORM=linux/amd64

RUN set +x; if [ "$TARGETPLATFORM" = "linux/arm64" ]; then \
      GOOS=linux GOARCH=arm64 go build -ldflags "-X github.com/entigolabs/entigo-infralib-agent/common.version=${VERSION} \
                           -X github.com/entigolabs/entigo-infralib-agent/common.buildDate=$(date -u +'%Y-%m-%dT%H:%M:%SZ') \
                           -X github.com/entigolabs/entigo-infralib-agent/common.gitCommit=${GITHUB_SHA} \
           -extldflags -static" -o bin/ei-agent main.go; \
    elif [ "$TARGETPLATFORM" = "linux/amd64" ]; then \
      GOOS=linux GOARCH=amd64 go build -ldflags "-X github.com/entigolabs/entigo-infralib-agent/common.version=${VERSION} \
                           -X github.com/entigolabs/entigo-infralib-agent/common.buildDate=$(date -u +'%Y-%m-%dT%H:%M:%SZ') \
                           -X github.com/entigolabs/entigo-infralib-agent/common.gitCommit=${GITHUB_SHA} \
          -linkmode external -extldflags -static" -o bin/ei-agent main.go; \
    fi
RUN find .

FROM --platform=${BUILDPLATFORM:-linux/amd64} alpine:3
WORKDIR /etc/ei-agent
COPY --from=build /go/ei-agent/bin/ei-agent /usr/bin/
CMD ei-agent run
