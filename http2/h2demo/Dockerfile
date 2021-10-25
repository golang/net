# Copyright 2018 The Go Authors. All rights reserved.
# Use of this source code is governed by a BSD-style
# license that can be found in the LICENSE file.

FROM golang:1.17 AS build
LABEL maintainer="golang-dev@googlegroups.com"

RUN mkdir /gocache
ENV GOCACHE /gocache

COPY go.mod /go/src/golang.org/x/build/go.mod
COPY go.sum /go/src/golang.org/x/build/go.sum

COPY . /go/src/golang.org/x/net

# Install binary to /go/bin:
WORKDIR /go/src/golang.org/x/net/http2/h2demo
RUN go install -tags "netgo"


FROM debian:stretch
RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates netbase \
    && rm -rf /var/lib/apt/lists/*

COPY --from=build /go/bin/h2demo /h2demo

