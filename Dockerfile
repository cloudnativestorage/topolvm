# Build topolvm
FROM --platform=$BUILDPLATFORM golang:1.25-bookworm AS build-topolvm

# Get argument
ARG TOPOLVM_VERSION
ARG TARGETARCH

COPY . /workdir
WORKDIR /workdir

RUN touch pkg/lvmd/proto/*.go
RUN make build-topolvm TOPOLVM_VERSION=${TOPOLVM_VERSION} GOARCH=${TARGETARCH}

# TopoLVM container
FROM ubuntu:22.04 AS topolvm

ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get -o Acquire::Retries=8 update \
    && apt-get -o Acquire::Retries=8 -y install --no-install-recommends \
        btrfs-progs \
        file \
        xfsprogs \
    && rm -rf /var/lib/apt/lists/*

COPY --from=build-topolvm /workdir/build/hypertopolvm /hypertopolvm

RUN ln -s hypertopolvm /lvmd \
    && ln -s hypertopolvm /topolvm-scheduler \
    && ln -s hypertopolvm /topolvm-node \
    && ln -s hypertopolvm /topolvm-controller \
    && ln -s hypertopolvm /topolvm-snapshotter

COPY --from=build-topolvm /workdir/LICENSE /LICENSE

ENTRYPOINT ["/hypertopolvm"]

# Build sidecars
FROM --platform=$BUILDPLATFORM build-topolvm AS build-sidecars

# Get argument
ARG TARGETARCH

ENV DEBIAN_FRONTEND=noninteractive
RUN  apt-get -o Acquire::Retries=8 update \
    && apt-get -o Acquire::Retries=8 -y install --no-install-recommends \
        patch

RUN make csi-sidecars GOARCH=${TARGETARCH}

# TopoLVM container with sidecar
FROM topolvm AS topolvm-with-sidecar

# Install curl and bzip2
RUN set -x \
  && apt-get -o Acquire::Retries=8 update \
  && apt-get -o Acquire::Retries=8 install -y --no-install-recommends apt-transport-https ca-certificates curl bzip2 \
  && rm -rf /var/lib/apt/lists/*

# Download and install the kubestash restic fork for the target architecture.
# The fork's asset naming (restic_linux_<arch>.bz2) has no version in the file
# name, and <arch> matches Docker's TARGETARCH (amd64/arm64).
ARG RESTIC_VERSION=0.18.1-20260421
ARG TARGETARCH
RUN set -x \
  && curl -fsSL -o /tmp/restic.bz2 https://github.com/kubestash/restic/releases/download/v${RESTIC_VERSION}/restic_linux_${TARGETARCH}.bz2 \
  && bzip2 -d /tmp/restic.bz2 \
  && mv /tmp/restic /bin/restic \
  && chmod 755 /bin/restic

COPY --from=build-sidecars /workdir/build/csi-provisioner /csi-provisioner
COPY --from=build-sidecars /workdir/build/csi-node-driver-registrar /csi-node-driver-registrar
COPY --from=build-sidecars /workdir/build/csi-resizer /csi-resizer
COPY --from=build-sidecars /workdir/build/csi-snapshotter /csi-snapshotter
COPY --from=build-sidecars /workdir/build/livenessprobe /livenessprobe
