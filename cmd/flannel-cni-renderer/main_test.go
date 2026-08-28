package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestSplitPodCIDR(t *testing.T) {
	tests := []struct {
		cidr                       string
		cniStart, cniEnd           string
		reservedStart, reservedEnd string
	}{
		{
			cidr: "10.244.17.0/24", cniStart: "10.244.17.2", cniEnd: "10.244.17.127",
			reservedStart: "10.244.17.128", reservedEnd: "10.244.17.254",
		},
		{
			cidr: "10.224.8.0/23", cniStart: "10.224.8.2", cniEnd: "10.224.8.255",
			reservedStart: "10.224.9.0", reservedEnd: "10.224.9.254",
		},
	}

	for _, test := range tests {
		t.Run(test.cidr, func(t *testing.T) {
			got, err := splitPodCIDR(test.cidr)
			if err != nil {
				t.Fatal(err)
			}
			if got.CNIStart.String() != test.cniStart || got.CNIEnd.String() != test.cniEnd ||
				got.ReservedStart.String() != test.reservedStart || got.ReservedEnd.String() != test.reservedEnd {
				t.Fatalf("splitPodCIDR(%q) = %+v", test.cidr, got)
			}
		})
	}
}

func TestSplitPodCIDRRejectsInvalidRanges(t *testing.T) {
	for _, value := range []string{"2001:db8::/64", "10.0.0.0/30", "not-a-cidr"} {
		if _, err := splitPodCIDR(value); err == nil {
			t.Errorf("splitPodCIDR(%q) unexpectedly succeeded", value)
		}
	}
}

func TestSelectIPv4PodCIDR(t *testing.T) {
	var node nodeResponse
	node.Spec.PodCIDR = "10.244.3.0/24"
	node.Spec.PodCIDRs = []string{"10.244.3.0/24", "fd00:10:244:3::/64"}
	got, err := selectIPv4PodCIDR(node)
	if err != nil {
		t.Fatal(err)
	}
	if got != "10.244.3.0/24" {
		t.Fatalf("selectIPv4PodCIDR() = %q", got)
	}
}

func TestRenderConflist(t *testing.T) {
	ranges, err := splitPodCIDR("10.244.17.0/24")
	if err != nil {
		t.Fatal(err)
	}
	contents, err := renderConflist(ranges)
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Plugins []struct {
			Type string `json:"type"`
			IPAM struct {
				RangeStart string `json:"rangeStart"`
				RangeEnd   string `json:"rangeEnd"`
			} `json:"ipam"`
		} `json:"plugins"`
	}
	if err := json.Unmarshal(contents, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Plugins) != 2 || decoded.Plugins[0].Type != "flannel" {
		t.Fatalf("unexpected plugins: %+v", decoded.Plugins)
	}
	if decoded.Plugins[0].IPAM.RangeStart != "10.244.17.2" || decoded.Plugins[0].IPAM.RangeEnd != "10.244.17.127" {
		t.Fatalf("unexpected IPAM: %+v", decoded.Plugins[0].IPAM)
	}
}

func TestAtomicWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "10-flannel.conflist")
	if err := atomicWrite(path, []byte("first\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := atomicWrite(path, []byte("second\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "second\n" {
		t.Fatalf("contents = %q", got)
	}
}
