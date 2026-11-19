package vens

import (
	"bytes"
	"encoding/binary"
	"testing"
	"time"
)

func TestPCReservePreservesPairedFields(t *testing.T) {
	template := bytes.Repeat([]byte{0xa5}, 128)
	binary.BigEndian.PutUint32(template[:4], 128)
	copy(template[4:8], Magic[:])
	binary.BigEndian.PutUint32(template[8:12], CmdReserve)
	binary.BigEndian.PutUint32(template[32:36], 0x00101e00)
	original := append([]byte(nil), template...)
	token := [8]byte{1, 2, 3, 4, 5, 6, 0, 0}
	ts := time.Date(2026, 11, 12, 17, 0, 0, 0, time.FixedZone("CET", 3600))
	packet, err := MarshalPCReserveFromTemplate(template, token, "192.0.2.10", 55265, ts)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(template, original) || len(packet) != 128 {
		t.Fatal("template mutated or packet resized")
	}
	if !bytes.Equal(packet[52:100], original[52:100]) || !bytes.Equal(packet[108:116], original[108:116]) {
		t.Fatal("paired identity fields changed")
	}
	if !bytes.Equal(packet[16:24], token[:]) || !bytes.Equal(packet[44:48], []byte{192, 0, 2, 10}) {
		t.Fatal("session transport fields not updated")
	}
	if binary.BigEndian.Uint16(packet[50:52]) != 55265 || binary.BigEndian.Uint16(packet[100:102]) != 2026 || int32(binary.BigEndian.Uint32(packet[116:120])) != -3600 {
		t.Fatal("notification port or time fields incorrect")
	}
}

func TestPCReserveRejectsUnknownTemplates(t *testing.T) {
	for _, template := range [][]byte{nil, make([]byte, 128), make([]byte, 384)} {
		if _, err := MarshalPCReserveFromTemplate(template, [8]byte{}, "192.0.2.10", 55265, time.Now()); err == nil {
			t.Fatal("unknown template accepted")
		}
	}
}
