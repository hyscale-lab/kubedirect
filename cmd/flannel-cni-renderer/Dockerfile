FROM golang:1.23 AS build

WORKDIR /src
COPY go.mod go.sum ./
COPY cmd/flannel-cni-renderer ./cmd/flannel-cni-renderer
RUN CGO_ENABLED=0 GOOS=linux go build -buildvcs=false -trimpath -ldflags="-s -w" \
    -o /out/flannel-cni-renderer ./cmd/flannel-cni-renderer
COPY cmd/kubedirect-host-local ./cmd/kubedirect-host-local
RUN CGO_ENABLED=0 GOOS=linux go build -buildvcs=false -trimpath -ldflags="-s -w" \
    -o /out/kubedirect-host-local ./cmd/kubedirect-host-local

FROM scratch
COPY --from=build /out/flannel-cni-renderer /flannel-cni-renderer
COPY --from=build /out/kubedirect-host-local /kubedirect-host-local
ENTRYPOINT ["/flannel-cni-renderer"]
