package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	defaultOutput     = "/etc/cni/net.d/10-flannel.conflist"
	defaultTokenFile  = "/var/run/secrets/kubernetes.io/serviceaccount/token"
	defaultCAFile     = "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"
	defaultIPAMSource = "/kubedirect-host-local"
)

type options struct {
	nodeName    string
	podCIDR     string
	output      string
	apiServer   string
	tokenFile   string
	caFile      string
	timeout     time.Duration
	installIPAM string
}

type nodeResponse struct {
	Spec struct {
		PodCIDR  string   `json:"podCIDR"`
		PodCIDRs []string `json:"podCIDRs"`
	} `json:"spec"`
}

type addressRange struct {
	PodCIDR       netip.Prefix
	CNIStart      netip.Addr
	CNIEnd        netip.Addr
	ReservedStart netip.Addr
	ReservedEnd   netip.Addr
}

type conflist struct {
	Name       string        `json:"name"`
	CNIVersion string        `json:"cniVersion"`
	Plugins    []interface{} `json:"plugins"`
}

type flannelPlugin struct {
	Type     string          `json:"type"`
	Delegate flannelDelegate `json:"delegate"`
	IPAM     hostLocalIPAM   `json:"ipam"`
}

type flannelDelegate struct {
	HairpinMode      bool `json:"hairpinMode"`
	IsDefaultGateway bool `json:"isDefaultGateway"`
}

type hostLocalIPAM struct {
	Type       string `json:"type"`
	RangeStart string `json:"rangeStart"`
	RangeEnd   string `json:"rangeEnd"`
}

type portmapPlugin struct {
	Type         string              `json:"type"`
	Capabilities portmapCapabilities `json:"capabilities"`
}

type portmapCapabilities struct {
	PortMappings bool `json:"portMappings"`
}

func main() {
	opts := parseFlags()

	ctx, cancel := context.WithTimeout(context.Background(), opts.timeout)
	defer cancel()

	podCIDR := opts.podCIDR
	if podCIDR == "" {
		if opts.nodeName == "" {
			log.Fatal("--node-name or NODE_NAME is required when --pod-cidr is not set")
		}
		var err error
		podCIDR, err = fetchIPv4PodCIDR(ctx, opts)
		if err != nil {
			log.Fatalf("discover PodCIDR for node %q: %v", opts.nodeName, err)
		}
	}

	ranges, err := splitPodCIDR(podCIDR)
	if err != nil {
		log.Fatalf("calculate address ranges: %v", err)
	}

	contents, err := renderConflist(ranges)
	if err != nil {
		log.Fatalf("render CNI conflist: %v", err)
	}
	if opts.installIPAM != "" {
		plugin, err := os.ReadFile(defaultIPAMSource)
		if err != nil {
			log.Fatalf("read bundled IPAM plugin: %v", err)
		}
		if err := atomicWrite(opts.installIPAM, plugin, 0o755); err != nil {
			log.Fatalf("install IPAM plugin: %v", err)
		}
		log.Printf("installed bounded host-local shim at %s", opts.installIPAM)
	}
	if opts.output == "-" {
		if _, err := os.Stdout.Write(contents); err != nil {
			log.Fatalf("write conflist to stdout: %v", err)
		}
	} else if err := atomicWrite(opts.output, contents, 0o644); err != nil {
		log.Fatalf("write CNI conflist: %v", err)
	}

	log.Printf("rendered %s: podCIDR=%s cni=%s-%s reserved=%s-%s",
		opts.output, ranges.PodCIDR, ranges.CNIStart, ranges.CNIEnd,
		ranges.ReservedStart, ranges.ReservedEnd)
}

func parseFlags() options {
	var opts options
	flag.StringVar(&opts.nodeName, "node-name", os.Getenv("NODE_NAME"), "Kubernetes Node name (defaults to NODE_NAME)")
	flag.StringVar(&opts.podCIDR, "pod-cidr", "", "IPv4 Node PodCIDR override; skips Kubernetes API discovery")
	flag.StringVar(&opts.output, "output", defaultOutput, "output conflist path, or - for stdout")
	flag.StringVar(&opts.apiServer, "api-server", defaultAPIServer(), "Kubernetes API server URL")
	flag.StringVar(&opts.tokenFile, "token-file", defaultTokenFile, "service-account token file")
	flag.StringVar(&opts.caFile, "ca-file", defaultCAFile, "Kubernetes API CA certificate file")
	flag.DurationVar(&opts.timeout, "timeout", 30*time.Second, "overall Kubernetes API request timeout")
	flag.StringVar(&opts.installIPAM, "install-ipam-plugin", "", "install the bundled bounded host-local shim at this path")
	flag.Parse()
	return opts
}

func defaultAPIServer() string {
	host := os.Getenv("KUBERNETES_SERVICE_HOST")
	port := os.Getenv("KUBERNETES_SERVICE_PORT_HTTPS")
	if port == "" {
		port = os.Getenv("KUBERNETES_SERVICE_PORT")
	}
	if host == "" || port == "" {
		return ""
	}
	return "https://" + netipHostPort(host, port)
}

func netipHostPort(host, port string) string {
	if strings.Contains(host, ":") && !strings.HasPrefix(host, "[") {
		return "[" + host + "]:" + port
	}
	return host + ":" + port
}

func fetchIPv4PodCIDR(ctx context.Context, opts options) (string, error) {
	if opts.apiServer == "" {
		return "", errors.New("--api-server is required; Kubernetes service environment variables are unset")
	}
	token, err := os.ReadFile(opts.tokenFile)
	if err != nil {
		return "", fmt.Errorf("read service-account token: %w", err)
	}
	ca, err := os.ReadFile(opts.caFile)
	if err != nil {
		return "", fmt.Errorf("read Kubernetes CA: %w", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca) {
		return "", fmt.Errorf("parse Kubernetes CA from %s", opts.caFile)
	}

	client := &http.Client{
		Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}},
	}
	endpoint := strings.TrimRight(opts.apiServer, "/") + "/api/v1/nodes/" + url.PathEscape(opts.nodeName)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("GET Node: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("GET Node returned %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}

	var node nodeResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&node); err != nil {
		return "", fmt.Errorf("decode Node: %w", err)
	}
	return selectIPv4PodCIDR(node)
}

func selectIPv4PodCIDR(node nodeResponse) (string, error) {
	candidates := append([]string(nil), node.Spec.PodCIDRs...)
	if node.Spec.PodCIDR != "" {
		candidates = append(candidates, node.Spec.PodCIDR)
	}
	seen := make(map[string]struct{})
	for _, candidate := range candidates {
		prefix, err := netip.ParsePrefix(candidate)
		if err != nil {
			return "", fmt.Errorf("parse Node PodCIDR %q: %w", candidate, err)
		}
		if !prefix.Addr().Is4() {
			continue
		}
		canonical := prefix.Masked().String()
		if _, ok := seen[canonical]; ok {
			continue
		}
		seen[canonical] = struct{}{}
		if len(seen) > 1 {
			return "", errors.New("Node has more than one IPv4 PodCIDR")
		}
	}
	for candidate := range seen {
		return candidate, nil
	}
	return "", errors.New("Node has no IPv4 PodCIDR")
}

func splitPodCIDR(value string) (addressRange, error) {
	prefix, err := netip.ParsePrefix(value)
	if err != nil {
		return addressRange{}, fmt.Errorf("parse %q: %w", value, err)
	}
	if !prefix.Addr().Is4() {
		return addressRange{}, fmt.Errorf("%s is not an IPv4 prefix", prefix)
	}
	prefix = prefix.Masked()
	// Each half needs room for ordinary addresses. Kubernetes normally assigns
	// /24s, but rejecting prefixes smaller than /29 prevents nonsensical ranges.
	if prefix.Bits() > 29 {
		return addressRange{}, fmt.Errorf("%s is too small to divide into CNI and reserved halves", prefix)
	}

	base := prefix.Addr().As4()
	baseUint := uint32(base[0])<<24 | uint32(base[1])<<16 | uint32(base[2])<<8 | uint32(base[3])
	hostBits := 32 - prefix.Bits()
	upperStart := baseUint + uint32(1)<<(hostBits-1)
	broadcast := baseUint + (uint32(1)<<hostBits - 1)

	return addressRange{
		PodCIDR:       prefix,
		CNIStart:      uint32Addr(baseUint + 2),
		CNIEnd:        uint32Addr(upperStart - 1),
		ReservedStart: uint32Addr(upperStart),
		ReservedEnd:   uint32Addr(broadcast - 1),
	}, nil
}

func uint32Addr(value uint32) netip.Addr {
	return netip.AddrFrom4([4]byte{byte(value >> 24), byte(value >> 16), byte(value >> 8), byte(value)})
}

func renderConflist(ranges addressRange) ([]byte, error) {
	conf := conflist{
		Name:       "cbr0",
		CNIVersion: "0.3.1",
		Plugins: []interface{}{
			flannelPlugin{
				Type: "flannel",
				Delegate: flannelDelegate{
					HairpinMode:      true,
					IsDefaultGateway: true,
				},
				IPAM: hostLocalIPAM{
					Type:       "kubedirect-host-local",
					RangeStart: ranges.CNIStart.String(),
					RangeEnd:   ranges.CNIEnd.String(),
				},
			},
			portmapPlugin{
				Type: "portmap",
				Capabilities: portmapCapabilities{
					PortMappings: true,
				},
			},
		},
	}
	contents, err := json.MarshalIndent(conf, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(contents, '\n'), nil
}

func atomicWrite(path string, contents []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create output directory: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".flannel-conflist-*")
	if err != nil {
		return fmt.Errorf("create temporary file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}()
	if err := tmp.Chmod(mode); err != nil {
		return fmt.Errorf("chmod temporary file: %w", err)
	}
	if _, err := tmp.Write(contents); err != nil {
		return fmt.Errorf("write temporary file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("sync temporary file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temporary file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("replace %s: %w", path, err)
	}

	dirHandle, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open output directory: %w", err)
	}
	defer dirHandle.Close()
	if err := dirHandle.Sync(); err != nil {
		return fmt.Errorf("sync output directory: %w", err)
	}
	return nil
}
