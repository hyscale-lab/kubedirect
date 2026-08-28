FROM golang:1.23 AS build

WORKDIR /src
COPY go.mod go.sum ./
COPY cmd/flannel-cni-renderer ./cmd/flannel-cni-renderer
RUN CGO_ENABLED=0 GOOS=linux go build -buildvcs=false -trimpath -ldflags="-s -w" \
    -o /out/flannel-cni-renderer ./cmd/flannel-cni-renderer

FROM scratch
COPY --from=build /out/flannel-cni-renderer /flannel-cni-renderer
ENTRYPOINT ["/flannel-cni-renderer"]
