FROM golang:1.18 as builder

WORKDIR /workspace
# Copy the Go Modules manifests
COPY go.mod go.mod
COPY go.sum go.sum
# cache deps before building and copying source so that we don't need to re-download as much
# and so that source changes don't invalidate our downloaded layer
#RUN go mod download

# Copy the go source
COPY apis/ apis/
COPY cmd/ cmd/
COPY webhook/ webhook/
COPY vendor/ vendor/

# Build
RUN CGO_ENABLED=0 GO111MODULE=on go build -mod=vendor -a -o cert-generator ./cmd/cert-generator/main.go

# Use distroless as minimal base image to package the manager binary
FROM ubuntu:focal
RUN apt-get update && \
  apt-get install --no-install-recommends -y ca-certificates iproute2 iptables && \
  apt-get clean && rm -rf  /var/log/*log /var/lib/apt/lists/* /var/log/apt/* /var/lib/dpkg/*-old /var/cache/debconf/*-old
WORKDIR /
COPY --from=builder /workspace/cert-generator .
COPY artifacts/scripts/ctrlmesh-init.sh /init.sh
ENTRYPOINT ["/init.sh"]
