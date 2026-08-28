# Flannel CNI renderer

This init-container command reserves the upper half of each physical Node's
IPv4 PodCIDR for Kubedirect. It fetches the Node through the Kubernetes API,
writes a bounded Flannel conflist, and installs the `kubedirect-host-local` IPAM
shim.

Flannel unconditionally generates an `ipam.ranges` entry covering the complete
Node PodCIDR. `host-local` gives that modern field precedence over legacy
top-level `rangeStart` and `rangeEnd` fields. The shim moves the bounds into the
generated IPv4 range immediately before invoking the real `host-local` binary.
It uses the same network name and host-local data directory, so existing
allocation records remain compatible.

For `10.244.17.0/24`, the result is:

- ordinary CNI allocations: `10.244.17.2` through `10.244.17.127`;
- Kubedirect reservations: `10.244.17.128` through `10.244.17.254`;
- `10.244.17.1` remains available to the CNI bridge;
- the subnet and broadcast addresses remain unused.

The renderer only configures ordinary CNI allocation. It does not yet allocate
or configure addresses for the custom kubelet.

## Test locally

```sh
go test ./cmd/flannel-cni-renderer ./cmd/kubedirect-host-local
go run ./cmd/flannel-cni-renderer \
  --pod-cidr=10.244.17.0/24 \
  --output=-
```

The command supports IPv4 Node PodCIDRs from `/8` through `/29`. Kubernetes
normally allocates a `/24` to each node. In a dual-stack cluster, the renderer
selects the Node's IPv4 PodCIDR and leaves IPv6 unconfigured.

## Build and publish the image

Run from the repository root, replacing the example registry and tag:

```sh
export RENDERER_IMAGE=registry.example.com/kubedirect/flannel-cni-renderer:v0.1.0
docker build \
  -f cmd/flannel-cni-renderer/Dockerfile \
  -t "$RENDERER_IMAGE" \
  .
docker push "$RENDERER_IMAGE"
```

For a multi-architecture cluster, publish a multi-platform image instead:

```sh
docker buildx build \
  --platform linux/amd64,linux/arm64 \
  -f cmd/flannel-cni-renderer/Dockerfile \
  -t "$RENDERER_IMAGE" \
  --push \
  .
```

Pin the deployed image by digest in production.

## Add it to the Flannel DaemonSet

Append the following container **after** Flannel's existing `install-cni`
init container. Init containers run in order, so the renderer must be last: it
replaces the static conflist installed by Flannel with the node-specific one.

```yaml
initContainers:
  # Existing install-cni-plugin and install-cni containers remain first.
  - name: render-cni
    image: registry.example.com/kubedirect/flannel-cni-renderer:v0.1.0
    imagePullPolicy: IfNotPresent
    args:
      - --output=/etc/cni/net.d/10-flannel.conflist
      - --install-ipam-plugin=/opt/cni/bin/kubedirect-host-local
    env:
      - name: NODE_NAME
        valueFrom:
          fieldRef:
            fieldPath: spec.nodeName
    securityContext:
      allowPrivilegeEscalation: false
      readOnlyRootFilesystem: true
      runAsNonRoot: false
      runAsUser: 0
      capabilities:
        drop: ["ALL"]
    volumeMounts:
      - name: cni
        mountPath: /etc/cni/net.d
      - name: cni-plugin
        mountPath: /opt/cni/bin
```

The existing Flannel ServiceAccount already needs `get` access to Nodes, which
is sufficient for the renderer. The projected service-account token and CA are
mounted automatically unless `automountServiceAccountToken` has been disabled.

For this repository, insert the container after `install-cni` in
`manifests/kubeadm/flannel.yaml`. Make the same change to
`manifests/kubeadm/flannel.large.yaml` when using the large-cluster manifest.

To patch an already running DaemonSet for a trial, substitute the image first:

```sh
export RENDERER_IMAGE=registry.example.com/kubedirect/flannel-cni-renderer:v0.1.0
envsubst < cmd/flannel-cni-renderer/render-init-container.patch.json | \
  kubectl -n kube-flannel patch daemonset kube-flannel-ds \
    --type=json --patch-file=/dev/stdin
kubectl -n kube-flannel rollout status daemonset/kube-flannel-ds
```

The JSON patch appends the renderer, so it assumes the existing Flannel init
containers are still present. A later re-application of the original Flannel
manifest will remove the trial patch; update the source manifest for permanent
deployment.

## Verify

Check one renderer log and the resulting host file:

```sh
kubectl -n kube-flannel logs <flannel-pod> -c render-cni
sudo cat /etc/cni/net.d/10-flannel.conflist
sudo test -x /opt/cni/bin/kubedirect-host-local
```

The log reports the complete Node PodCIDR, the lower-half CNI range, and the
upper-half reserved range. Before rolling this onto a node that already has
pods, verify that no existing pod address lies in the upper half. This change
prevents new allocations there but does not migrate or invalidate existing
host-local allocations. The Flannel delegate configuration may still display
the full-subnet `ranges` value because Flannel creates it dynamically; the IPAM
shim constrains that value before the real host-local allocator sees it.
