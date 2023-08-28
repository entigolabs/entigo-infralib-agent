FROM golang:1.20-buster AS build
WORKDIR /go/ei-agent
COPY go.* ./
RUN go mod download
COPY . ./
RUN go build \
    -o bin/ei-agent \
    -ldflags "-linkmode external -extldflags -static" \
    main.go

FROM alpine:3
COPY --from=build /go/ei-agent/bin/ei-agent /usr/bin/
CMD ei-agent run