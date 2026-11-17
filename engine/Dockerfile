# SPDX-License-Identifier: Apache-2.0
# Copyright Contributors to the project. See NOTICE.

# Engine, relay and projector of the deployment: Go + libcbv2g (EXI codec of EVerest, Apache 2.0).
# Build context: engine/.
FROM golang:1.27-bookworm AS build
ARG LIBCBV2G_TAG=v0.3.2
RUN apt-get update && apt-get install -y --no-install-recommends cmake ninja-build \
    && rm -rf /var/lib/apt/lists/*
RUN git clone --depth 1 --branch ${LIBCBV2G_TAG} https://github.com/EVerest/libcbv2g.git /src/libcbv2g \
    && cmake -S /src/libcbv2g -B /src/libcbv2g/build -G Ninja -DCMAKE_BUILD_TYPE=Release \
       -DCMAKE_POSITION_INDEPENDENT_CODE=ON -DBUILD_SHARED_LIBS=OFF -DCMAKE_INSTALL_PREFIX=/opt/cbv2g \
    && cmake --build /src/libcbv2g/build && cmake --install /src/libcbv2g/build

WORKDIR /src/server
COPY go.mod go.sum ./
COPY cmd ./cmd
COPY internal ./internal
ENV CGO_ENABLED=1 CGO_CFLAGS="-I/opt/cbv2g/include" CGO_LDFLAGS="-L/opt/cbv2g/lib"
RUN go mod download \
    && go test ./... \
    && go build -trimpath -ldflags="-s -w" -o /out/engine ./cmd/engine \
    && go build -trimpath -ldflags="-s -w" -o /out/relay ./cmd/relay \
    && go build -trimpath -ldflags="-s -w" -o /out/projector ./cmd/projector

FROM debian:bookworm-slim
COPY --from=build /out/engine /out/relay /out/projector /usr/local/bin/
USER nobody
ENTRYPOINT ["/usr/local/bin/engine"]
