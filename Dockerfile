FROM golang:1.21-alpine AS build
WORKDIR /go/ei-agent
COPY go.* ./
RUN go mod download
COPY . ./
RUN go build -v -o bin/ei-agent main.go

FROM alpine:3
WORKDIR /etc/ei-agent
COPY --from=build /go/ei-agent/base.tf /etc/ei-agent/
COPY --from=build /go/ei-agent/eks.tf /etc/ei-agent/
COPY --from=build /go/ei-agent/bin/ei-agent /usr/bin/
CMD ei-agent run