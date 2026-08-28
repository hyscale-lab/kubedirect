package main

import (
	"encoding/json"
	"testing"
)

func TestConstrainRanges(t *testing.T) {
	input := []byte(`{
  "name":"cbr0",
  "cniVersion":"0.3.1",
  "ipam":{
    "rangeEnd":"10.168.13.255",
    "rangeStart":"10.168.12.2",
    "ranges":[[{"subnet":"10.168.12.0/22"}]],
    "routes":[{"dst":"10.168.0.0/16"}],
    "type":"kubedirect-host-local"
  }
}`)
	output, err := constrainRanges(input)
	if err != nil {
		t.Fatal(err)
	}
	var config struct {
		IPAM struct {
			Type       string `json:"type"`
			RangeStart string `json:"rangeStart"`
			RangeEnd   string `json:"rangeEnd"`
			Ranges     [][]struct {
				Subnet     string `json:"subnet"`
				RangeStart string `json:"rangeStart"`
				RangeEnd   string `json:"rangeEnd"`
			} `json:"ranges"`
		} `json:"ipam"`
	}
	if err := json.Unmarshal(output, &config); err != nil {
		t.Fatal(err)
	}
	if config.IPAM.Type != "host-local" {
		t.Fatalf("IPAM type = %q", config.IPAM.Type)
	}
	if config.IPAM.RangeStart != "" || config.IPAM.RangeEnd != "" {
		t.Fatalf("legacy top-level bounds remain: %+v", config.IPAM)
	}
	got := config.IPAM.Ranges[0][0]
	if got.Subnet != "10.168.12.0/22" || got.RangeStart != "10.168.12.2" || got.RangeEnd != "10.168.13.255" {
		t.Fatalf("bounded range = %+v", got)
	}
}

func TestConstrainRangesFailsClosed(t *testing.T) {
	tests := []string{
		`{"ipam":{"ranges":[[{"subnet":"10.168.12.0/22"}]]}}`,
		`{"ipam":{"rangeStart":"10.168.12.2","rangeEnd":"10.168.15.254"}}`,
		`{"ipam":{"rangeStart":"10.168.12.2","rangeEnd":"10.168.15.254","ranges":[[{"subnet":"10.0.0.0/24"}]]}}`,
	}
	for _, input := range tests {
		if _, err := constrainRanges([]byte(input)); err == nil {
			t.Errorf("constrainRanges(%s) unexpectedly succeeded", input)
		}
	}
}
