package ttrpc

import (
	"testing"

	"github.com/gogo/protobuf/proto"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestResponseStatusUsesGogoCompatibleWireType(t *testing.T) {
	want := status.New(codes.DeadlineExceeded, "VM creation timed out").Proto()
	wire, err := proto.Marshal(&Response{Status: statusToWire(want)})
	if err != nil {
		t.Fatal(err)
	}

	var response Response
	if err := proto.Unmarshal(wire, &response); err != nil {
		t.Fatal(err)
	}
	got := statusFromWire(response.Status)
	if got.Code != want.Code || got.Message != want.Message {
		t.Fatalf("decoded status = (%d, %q), want (%d, %q)", got.Code, got.Message, want.Code, want.Message)
	}
}
