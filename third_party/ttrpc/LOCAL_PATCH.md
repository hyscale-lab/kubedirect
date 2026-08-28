# Local ttrpc compatibility patch

This directory is based on `github.com/containerd/ttrpc` **v1.1.2**, the
version selected by the pinned vHive/firecracker-containerd dependency set.

The response envelope converts `google.rpc.Status` to the gogo-generated wire
type before gogo reflection. This fixes containerd/ttrpc issue #62 when ttrpc
v1.1.2 is linked into a process that also uses a modern protobuf-generated
`google.rpc.Status`. Request and response payloads remain on gogo protobuf for
compatibility with the pinned Firecracker control API.
