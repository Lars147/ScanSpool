package vens

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"testing"
	"time"
)

func TestRejectedScanSetupEndsTransaction(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	result := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			result <- err
			return
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(5 * time.Second))
		response := func(size int) []byte {
			p := make([]byte, size)
			binary.BigEndian.PutUint32(p[:4], uint32(size))
			copy(p[4:8], Magic[:])
			return p
		}
		conn.Write(response(16))
		for step, opcode := range []byte{0xd8, 0xd4, 0xd5, 0x0a, 0xe0, 0x03, 0xd6} {
			header := make([]byte, 4)
			if _, err := io.ReadFull(conn, header); err != nil {
				result <- fmt.Errorf("step %d: %w", step, err)
				return
			}
			size := binary.BigEndian.Uint32(header)
			request := make([]byte, size)
			copy(request, header)
			if _, err := io.ReadFull(conn, request[4:]); err != nil {
				result <- err
				return
			}
			actual := request[48]
			if opcode == 0x0a {
				actual = byte(binary.BigEndian.Uint32(request[32:36]))
			}
			if actual != opcode {
				result <- fmt.Errorf("step %d opcode %02x, want %02x", step, actual, opcode)
				return
			}
			reply := response(64)
			if opcode == 0xe0 {
				binary.BigEndian.PutUint32(reply[12:16], 2)
			}
			if opcode == 0x03 {
				reply[42], reply[52], reply[53] = 5, 0x2c, 2
			}
			if _, err := conn.Write(reply); err != nil {
				result <- err
				return
			}
		}
		result <- nil
	}()
	port := uint16(listener.Addr().(*net.TCPAddr).Port)
	channel := NewDataChannel("127.0.0.1", port, [8]byte{})
	if _, err := channel.StartScan(DefaultScanConfig()); err == nil {
		t.Fatal("rejected setup reported as successful")
	}
	if err := <-result; err != nil {
		t.Fatal(err)
	}
}
