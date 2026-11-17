# SPDX-License-Identifier: Apache-2.0
# Copyright Contributors to the project. See NOTICE.

# Fleet generator (load tests), built from engine/: docker build -f cmd/fleetgen/Dockerfile -t fleetgen engine
FROM golang:1.27-bookworm AS build
ARG LIBCBV2G_TAG=v0.3.2
RUN apt-get update && apt-get install -y --no-install-recommends cmake ninja-build \
    && rm -rf /var/lib/apt/lists/*
RUN git clone --depth 1 --branch ${LIBCBV2G_TAG} https://github.com/EVerest/libcbv2g.git /src/libcbv2g \
    && cmake -S /src/libcbv2g -B /src/libcbv2g/build -G Ninja -DCMAKE_BUILD_TYPE=Release \
       -DCMAKE_POSITION_INDEPENDENT_CODE=ON -DBUILD_SHARED_LIBS=OFF -DCMAKE_INSTALL_PREFIX=/opt/cbv2g \
    && cmake --build /src/libcbv2g/build && cmake --install /src/libcbv2g/build

WORKDIR /src/engine
COPY . .
ENV CGO_ENABLED=1 CGO_CFLAGS="-I/opt/cbv2g/include" CGO_LDFLAGS="-L/opt/cbv2g/lib"
RUN go mod download \
    && go test ./internal/evexi/ \
    && go build -trimpath -ldflags="-s -w" -o /out/fleetgen ./cmd/fleetgen

FROM debian:bookworm-slim
COPY --from=build /out/fleetgen /usr/local/bin/fleetgen
USER nobody
WORKDIR /tmp
ENTRYPOINT ["/usr/local/bin/fleetgen"]
