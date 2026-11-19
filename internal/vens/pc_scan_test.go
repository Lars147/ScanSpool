package vens

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/jpeg"
	"io"
	"net"
	"testing"
)

func TestPCPageReassemblesFramesWithoutAnotherReadRequest(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	var encoded bytes.Buffer
	if err := jpeg.Encode(&encoded, image.NewGray(image.Rect(0, 0, 4, 4)), nil); err != nil {
		t.Fatal(err)
	}
	want := encoded.Bytes()
	result := make(chan error, 1)
	go func() {
		defer server.Close()
		request := make([]byte, 64)
		if _, err := io.ReadFull(server, request); err != nil {
			result <- err
			return
		}
		if request[48] != 0x28 || binary.BigEndian.Uint32(request[36:40]) != 0x300000 {
			result <- io.ErrUnexpectedEOF
			return
		}
		for _, chunk := range [][]byte{want[:len(want)/2], want[len(want)/2:]} {
			frame := make([]byte, 42+len(chunk))
			binary.BigEndian.PutUint32(frame[:4], uint32(len(frame)))
			copy(frame[4:8], Magic[:])
			copy(frame[42:], chunk)
			if _, err := server.Write(frame); err != nil {
				result <- err
				return
			}
		}
		if _, err := io.ReadFull(server, request); err != nil {
			result <- err
			return
		}
		if request[48] != 0x03 {
			result <- io.ErrUnexpectedEOF
			return
		}
		sense := make([]byte, 64)
		binary.BigEndian.PutUint32(sense[:4], 64)
		copy(sense[4:8], Magic[:])
		_, err := server.Write(sense)
		result <- err
	}()
	session := &ScanSession{conn: client, pcMode: true, sidesPerSheet: 2, scanConfig: PCReferenceConfig()}
	page, err := session.nextPCPage()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(page.JPEG, want) || page.Side != 0 || session.transferSheet != 1 {
		t.Fatal("JPEG frames or page counters incorrect")
	}
	if err := <-result; err != nil {
		t.Fatal(err)
	}
}
