FROM golang:1.16 as builder

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
COPY client/ client/
COPY proxy/ proxy/
COPY util/ util/
COPY vendor/ vendor/

# Build
RUN CGO_ENABLED=0 GO111MODULE=on go build -mod=vendor -a -o ctrlmesh-proxy ./cmd/proxy/main.go

# Use distroless as minimal base image to package the manager binary
FROM ubuntu:focal
RUN apt-get update && \
  apt-get install --no-install-recommends -y \
  ca-certificates \
  curl \
  iputils-ping \
  tcpdump \
  iproute2 \
  iptables \
  net-tools \
  telnet \
  lsof \
  linux-tools-generic \
  sudo && \
  apt-get clean && \
  rm -rf  /var/log/*log /var/lib/apt/lists/* /var/log/apt/* /var/lib/dpkg/*-old /var/cache/debconf/*-old

# Sudoers used to allow tcpdump and other debug utilities.
RUN useradd -m --uid 1359 ctrlmesh-proxy && \
  echo "ctrlmesh-proxy ALL=NOPASSWD: ALL" >> /etc/sudoers
WORKDIR /
COPY artifacts/scripts/ctrlmesh-proxy-poststart.sh /poststart.sh
COPY --from=builder /workspace/ctrlmesh-proxy .
ENTRYPOINT ["/ctrlmesh-proxy"]
