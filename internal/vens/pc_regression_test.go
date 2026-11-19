package vens

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"image"
	"image/jpeg"
	"io"
	"net"
	"scanspool/profiles"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestPCPrepareConnectionFailureReleasesTransaction(t *testing.T) {
	listener, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	listener.SetDeadline(time.Now().Add(3 * time.Second))
	cleanup := make(chan bool, 1)
	go func() {
		frame := func(size int) []byte {
			out := make([]byte, size)
			binary.BigEndian.PutUint32(out, uint32(size))
			copy(out[4:8], Magic[:])
			return out
		}
		first, err := listener.AcceptTCP()
		if err != nil {
			cleanup <- false
			return
		}
		first.SetDeadline(time.Now().Add(time.Second))
		first.Write(frame(16))
		header := make([]byte, 4)
		io.ReadFull(first, header)
		request := make([]byte, binary.BigEndian.Uint32(header)-4)
		io.ReadFull(first, request)
		first.Write(frame(64))
		first.Close()
		second, err := listener.AcceptTCP()
		if err != nil {
			cleanup <- false
			return
		}
		// Preparation succeeded; the scan connection now fails its welcome.
		second.Write(make([]byte, 16))
		second.Close()
		listener.SetDeadline(time.Now().Add(300 * time.Millisecond))
		third, err := listener.AcceptTCP()
		if err != nil {
			cleanup <- false
			return
		}
		defer third.Close()
		third.SetDeadline(time.Now().Add(time.Second))
		third.Write(frame(16))
		io.ReadFull(third, header)
		request = make([]byte, binary.BigEndian.Uint32(header)-4)
		io.ReadFull(third, request)
		third.Write(frame(64))
		cleanup <- len(request) > 44 && request[44] == 0xd6
	}()
	profile := profiles.IX1300()
	channel := NewDataChannel("127.0.0.1", uint16(listener.Addr().(*net.TCPAddr).Port), [8]byte{}, profile)
	if _, err := channel.StartScan(PCReferenceConfig()); err == nil {
		t.Fatal("expected failed welcome")
	}
	if !<-cleanup {
		t.Fatal("D5 preparation succeeded, but no D6 cleanup was sent after scan connection failed")
	}
}

func TestPCTerminalSensePropagatesPaperJam(t *testing.T) {
	listener, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	listener.SetDeadline(time.Now().Add(3 * time.Second))
	frame := func(size int) []byte {
		out := make([]byte, size)
		binary.BigEndian.PutUint32(out, uint32(size))
		copy(out[4:8], Magic[:])
		return out
	}
	go func() {
		conn, err := listener.AcceptTCP()
		if err != nil {
			return
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(time.Second))
		conn.Write(frame(16))
		request := make([]byte, 64)
		io.ReadFull(conn, request)
		status := frame(88)
		status[43] = 0x80 // Empty feeder at end of a partially completed batch.
		conn.Write(status)
	}()
	client, server := net.Pipe()
	defer client.Close()
	go func() {
		defer server.Close()
		server.SetDeadline(time.Now().Add(time.Second))
		request := make([]byte, 64)
		io.ReadFull(server, request) // E0.
		server.Write(frame(64))
		io.ReadFull(server, request) // REQUEST SENSE.
		sense := frame(64)
		sense[SenseDataOffset+2] = SenseKeyMediumError
		sense[SenseDataOffset+12] = VendorASC
		sense[SenseDataOffset+13] = ASCQPaperJam
		server.Write(sense)
	}()
	channel := NewDataChannel("127.0.0.1", uint16(listener.Addr().(*net.TCPAddr).Port), [8]byte{})
	session := &ScanSession{conn: client, dc: channel, pcMode: true, physicalSheet: 1}
	_, err = session.NextPage()
	var scanErr *ScanError
	if !errors.As(err, &scanErr) || scanErr.Kind != ScanErrPaperJam {
		t.Fatalf("terminal paper jam became successful batch completion: %v", err)
	}
}

func TestPCSenseRejectsMalformedAndErrorStatuses(t *testing.T) {
	for _, n := range []int{0, 16, SenseDataOffset + 13} {
		if parsePCSense(make([]byte, n)) == nil {
			t.Fatalf("accepted %d-byte sense", n)
		}
	}
	response := make([]byte, 64)
	copy(response[4:8], Magic[:])
	binary.BigEndian.PutUint32(response[12:16], 7)
	if parsePCSense(response) == nil {
		t.Fatal("accepted failed command status")
	}
	response[15] = 2
	response[42], response[52], response[53] = SenseKeyMediumError, VendorASC, ASCQScanComplete
	if err := parsePCSense(response); err != nil {
		t.Fatal(err)
	}
}

func TestPCClosePreparedWithoutConnectionIsIdempotent(t *testing.T) {
	listener, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	listener.SetDeadline(time.Now().Add(3 * time.Second))
	completed := make(chan error, 1)
	go func() {
		conn, err := listener.AcceptTCP()
		if err != nil {
			completed <- err
			return
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(time.Second))
		frame := make([]byte, 64)
		binary.BigEndian.PutUint32(frame, 16)
		copy(frame[4:8], Magic[:])
		conn.Write(frame[:16])
		req := make([]byte, 64)
		_, err = io.ReadFull(conn, req)
		if err == nil && req[48] != 0xd6 {
			err = errors.New("expected D6")
		}
		binary.BigEndian.PutUint32(frame, 64)
		binary.BigEndian.PutUint32(frame[12:16], 3)
		conn.Write(frame)
		completed <- err
	}()
	session := &ScanSession{pcMode: true, pcPrepared: true, dc: NewDataChannel("127.0.0.1", uint16(listener.Addr().(*net.TCPAddr).Port), [8]byte{})}
	first := session.Close()
	if first == nil {
		t.Fatal("failed D6 treated as success")
	}
	if second := session.Close(); second != first {
		t.Fatal("close did not retain its error")
	}
	if err := <-completed; err != nil {
		t.Fatal(err)
	}
}

// pcTestChannel emulates only the captured command ordering on loopback.
func pcTestChannel(t *testing.T, losePrepare bool, endStatus uint32) (*DataChannel, *atomic.Int32) {
	t.Helper()
	listener, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	var workers sync.WaitGroup
	done := make(chan struct{})
	ends := &atomic.Int32{}
	var data bytes.Buffer
	if err := jpeg.Encode(&data, image.NewGray(image.Rect(0, 0, 4, 4)), nil); err != nil {
		t.Fatal(err)
	}
	frame := func(size int) []byte {
		b := make([]byte, size)
		binary.BigEndian.PutUint32(b, uint32(size))
		copy(b[4:8], Magic[:])
		return b
	}
	go func() {
		defer close(done)
		for {
			conn, err := listener.AcceptTCP()
			if err != nil {
				return
			}
			workers.Add(1)
			go func() {
				defer workers.Done()
				defer conn.Close()
				conn.SetDeadline(time.Now().Add(3 * time.Second))
				conn.Write(frame(16))
				for {
					header := make([]byte, 4)
					if _, err := io.ReadFull(conn, header); err != nil {
						return
					}
					size := int(binary.BigEndian.Uint32(header))
					if size < 64 || size > 1024 {
						return
					}
					request := make([]byte, size)
					copy(request, header)
					if _, err := io.ReadFull(conn, request[4:]); err != nil {
						return
					}
					response := frame(64)
					switch request[48] {
					case 0xd5:
						if losePrepare {
							return
						}
					case 0xd6:
						ends.Add(1)
						binary.BigEndian.PutUint32(response[12:16], endStatus)
					case 0xc2:
						response = frame(88)
						response[43] = 0x80
					case 0x28:
						response = frame(42 + data.Len())
						copy(response[42:], data.Bytes())
					}
					if _, err := conn.Write(response); err != nil {
						return
					}
				}
			}()
		}
	}()
	t.Cleanup(func() { listener.Close(); <-done; workers.Wait() })
	return NewDataChannel("127.0.0.1", uint16(listener.Addr().(*net.TCPAddr).Port), [8]byte{}, profiles.IX1300()), ends
}
func TestPCLostPreparationResponseStillEndsTransaction(t *testing.T) {
	channel, ends := pcTestChannel(t, true, 0)
	if _, err := channel.StartScan(PCReferenceConfig()); err == nil {
		t.Fatal("lost response became success")
	}
	if ends.Load() != 1 {
		t.Fatal("missing D6 after lost D5 response", ends.Load())
	}
}
func TestPCEndFailureCannotCompleteCollectedScan(t *testing.T) {
	channel, ends := pcTestChannel(t, false, 7)
	pages, err := channel.RunScan(PCReferenceConfig(), nil)
	if len(pages) != 2 || err == nil || err == io.EOF {
		t.Fatal("D6 failure became complete scan", len(pages), err)
	}
	if ends.Load() != 1 {
		t.Fatal("cleanup repeated", ends.Load())
	}
}
func TestPCCancelledContextStillAllowsCleanup(t *testing.T) {
	channel, ends := pcTestChannel(t, false, 0)
	ctx, cancel := context.WithCancel(context.Background())
	channel.WithContext(ctx)
	session, err := channel.StartScan(PCReferenceConfig())
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := session.Close(); err != nil {
		t.Fatal("cancelled scan prevented D6", err)
	}
	if ends.Load() != 1 {
		t.Fatal("cleanup missing", ends.Load())
	}
}
