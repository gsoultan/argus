package agent

import (
	"bytes"
	"testing"
)

func TestAVCCBecomesAnnexBWithTheParameterSetsFirst(t *testing.T) {
	sps, pps := []byte{0x67, 1, 2}, []byte{0x68, 3}
	header := annexBNAL(annexBNAL(nil, sps), pps)
	idr := []byte{0x65, 9, 9, 9}
	avcc := append([]byte{0, 0, 0, byte(len(idr))}, idr...)
	got, err := avccToAnnexB(nil, header, avcc, 4)
	if err != nil {
		t.Fatal(err)
	}
	want := []byte{0, 0, 0, 1, 0x67, 1, 2, 0, 0, 0, 1, 0x68, 3, 0, 0, 0, 1, 0x65, 9, 9, 9}
	if !bytes.Equal(got, want) {
		t.Fatalf("got % x\nwant % x", got, want)
	}
	if !h264HasIDR(got) || !h264HasNAL(got, 7) {
		t.Fatal("the key frame does not read as one")
	}
	if _, err := avccToAnnexB(nil, nil, []byte{0, 0, 0, 9, 0x65}, 4); err == nil {
		t.Fatal("a NAL longer than the data was taken")
	}
	if _, err := avccToAnnexB(nil, nil, []byte{0, 0}, 4); err == nil {
		t.Fatal("a truncated length was taken")
	}
}
