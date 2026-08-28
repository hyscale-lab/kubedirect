package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"os/exec"
)

const realHostLocal = "/opt/cni/bin/host-local"

func main() {
	input, err := io.ReadAll(io.LimitReader(os.Stdin, 4<<20))
	if err != nil {
		fail("read CNI configuration", err)
	}
	config, err := constrainRanges(input)
	if err != nil {
		fail("constrain host-local ranges", err)
	}

	cmd := exec.Command(realHostLocal, os.Args[1:]...)
	cmd.Env = os.Environ()
	cmd.Stdin = bytes.NewReader(config)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			os.Exit(exitErr.ExitCode())
		}
		fail("execute host-local", err)
	}
}

func fail(operation string, err error) {
	message := fmt.Sprintf("kubedirect-host-local: %s: %v", operation, err)
	_ = json.NewEncoder(os.Stdout).Encode(map[string]interface{}{
		"cniVersion": "0.3.1",
		"code":       100,
		"msg":        message,
	})
	fmt.Fprintln(os.Stderr, message)
	os.Exit(1)
}

func constrainRanges(input []byte) ([]byte, error) {
	var config map[string]interface{}
	decoder := json.NewDecoder(bytes.NewReader(input))
	decoder.UseNumber()
	if err := decoder.Decode(&config); err != nil {
		return nil, fmt.Errorf("decode CNI configuration: %w", err)
	}
	ipam, ok := config["ipam"].(map[string]interface{})
	if !ok {
		return nil, errors.New("configuration has no ipam object")
	}
	start, err := parseBound(ipam, "rangeStart")
	if err != nil {
		return nil, err
	}
	end, err := parseBound(ipam, "rangeEnd")
	if err != nil {
		return nil, err
	}
	if !start.Is4() || !end.Is4() || end.Less(start) {
		return nil, fmt.Errorf("invalid IPv4 allocation bounds %s-%s", start, end)
	}

	rangeSets, ok := ipam["ranges"].([]interface{})
	if !ok || len(rangeSets) == 0 {
		return nil, errors.New("Flannel delegate configuration has no ranges array")
	}
	bounded := 0
	for setIndex, rawSet := range rangeSets {
		set, ok := rawSet.([]interface{})
		if !ok || len(set) == 0 {
			return nil, fmt.Errorf("ranges[%d] is not a non-empty range set", setIndex)
		}
		for rangeIndex, rawRange := range set {
			allocationRange, ok := rawRange.(map[string]interface{})
			if !ok {
				return nil, fmt.Errorf("ranges[%d][%d] is not an object", setIndex, rangeIndex)
			}
			subnetValue, ok := allocationRange["subnet"].(string)
			if !ok {
				return nil, fmt.Errorf("ranges[%d][%d] has no subnet", setIndex, rangeIndex)
			}
			subnet, err := netip.ParsePrefix(subnetValue)
			if err != nil {
				return nil, fmt.Errorf("parse ranges[%d][%d].subnet: %w", setIndex, rangeIndex, err)
			}
			if !subnet.Addr().Is4() {
				continue
			}
			if !subnet.Contains(start) || !subnet.Contains(end) {
				return nil, fmt.Errorf("bounds %s-%s are outside Flannel subnet %s", start, end, subnet)
			}
			allocationRange["rangeStart"] = start.String()
			allocationRange["rangeEnd"] = end.String()
			bounded++
		}
	}
	if bounded != 1 {
		return nil, fmt.Errorf("expected exactly one IPv4 Flannel range, found %d", bounded)
	}

	delete(ipam, "rangeStart")
	delete(ipam, "rangeEnd")
	ipam["type"] = "host-local"
	output, err := json.Marshal(config)
	if err != nil {
		return nil, fmt.Errorf("encode CNI configuration: %w", err)
	}
	return output, nil
}

func parseBound(ipam map[string]interface{}, name string) (netip.Addr, error) {
	value, ok := ipam[name].(string)
	if !ok || value == "" {
		return netip.Addr{}, fmt.Errorf("ipam.%s is required", name)
	}
	address, err := netip.ParseAddr(value)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("parse ipam.%s: %w", name, err)
	}
	return address, nil
}
