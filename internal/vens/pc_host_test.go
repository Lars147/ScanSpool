package vens

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

func TestGeneratedPCReserveMatchesManufacturerFields(t *testing.T) {
	info := DeviceInfo{Name: "ScanSnap iX1300", InterfaceVersion: 16, PasswordRequired: true}
	template, err := PCReserveTemplate(info, 0x0102030405060708, "0700")
	if err != nil {
		t.Fatal(err)
	}
	if len(template) != 128 || !bytes.Equal(template[32:44], []byte{0, 0x10, 0x1e, 0, 0, 0, 0, 0, 0, 0, 0, 1}) {
		t.Fatal("incorrect interface/timeout/mode/password fields")
	}
	if string(template[52:64]) != "171136176174" || !bytes.Equal(template[64:100], make([]byte, 36)) {
		t.Fatal("password encoding differs from pure manufacturer serializer")
	}
	if binary.BigEndian.Uint64(template[108:116]) != 0x0102030405060708 {
		t.Fatal("host ID not big-endian")
	}
	if _, err := MarshalPCReserveFromTemplate(template, [8]byte{}, "192.0.2.1", 55265, time.Now()); err != nil {
		t.Fatal(err)
	}
	for _, password := range []string{"", "0123456789abcdefg", "über"} {
		if _, err := PCReserveTemplate(info, 1, password); err == nil {
			t.Fatal("unsupported password accepted")
		}
	}
}

type hostSimulator struct {
	mu              sync.Mutex
	table           []byte
	clears          int
	writes          int
	clearAckFailure bool
	badLength       bool
}

func simulatedHosts(t *testing.T, table []byte) (*PCHostSession, *hostSimulator) {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	device := &hostSimulator{table: bytes.Clone(table)}
	var workers sync.WaitGroup
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			workers.Add(1)
			go func() { defer workers.Done(); defer connection.Close(); device.handle(connection) }()
		}
	}()
	t.Cleanup(func() { listener.Close(); <-finished; workers.Wait() })
	port := uint16(listener.Addr().(*net.TCPAddr).Port)
	return &PCHostSession{data: NewDataChannel("127.0.0.1", port, [8]byte{}), token: [8]byte{}}, device
}

func (s *hostSimulator) handle(connection net.Conn) {
	connection.SetDeadline(time.Now().Add(2 * time.Second))
	welcome := make([]byte, 16)
	binary.BigEndian.PutUint32(welcome, 16)
	copy(welcome[4:], Magic[:])
	connection.Write(welcome)
	length := make([]byte, 4)
	if _, err := io.ReadFull(connection, length); err != nil {
		return
	}
	size := binary.BigEndian.Uint32(length)
	if size < 64 || size > 1024 {
		return
	}
	request := make([]byte, size)
	copy(request, length)
	if _, err := io.ReadFull(connection, request[4:]); err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var payload []byte
	status := uint32(0)
	if request[48] != 0xdc {
		status = 1
	} else {
		switch request[50] {
		case 4:
			payload = make([]byte, 4)
			n := len(s.table)
			if s.badLength {
				n = 65535
			}
			binary.BigEndian.PutUint16(payload, uint16(n))
		case 1:
			payload = bytes.Clone(s.table)
		case 3:
			if !bytes.Equal(request[48:56], []byte{0xdc, 0, 3, 0, 0, 0, 0, 2}) {
				status = 1
				break
			}
			s.clears++
			s.table = make([]byte, 8)
			payload = []byte{0, 0}
			if s.clearAckFailure {
				payload[0] = 0xff
			}
		case 0:
			if len(request) != 80 || !bytes.Equal(request[48:56], []byte{0xdc, 0, 0, 0, 0, 16, 0, 2}) {
				status = 1
				break
			}
			if s.table[0] != 0 {
				payload = []byte{0xff, 0}
				break
			}
			s.writes++
			s.table = append([]byte{1, 0, 0, 0, 0, 0, 0, 0}, request[64:80]...)
			payload = []byte{0, 0}
		default:
			status = 1
		}
	}
	response := make([]byte, 40+len(payload))
	binary.BigEndian.PutUint32(response, uint32(len(response)))
	copy(response[4:], Magic[:])
	binary.BigEndian.PutUint32(response[12:16], status)
	copy(response[40:], payload)
	connection.Write(response)
}

func hostTable(id uint64) []byte {
	table := make([]byte, 24)
	table[0] = 1
	binary.BigEndian.PutUint64(table[8:16], id)
	return table
}

func TestHostSwitchAndRestoreOverRealTCP(t *testing.T) {
	before := hostTable(1)
	session, device := simulatedHosts(t, before)
	newRecord := hostTable(2)[8:]
	if err := session.ReplaceHost(before, newRecord); err != nil {
		t.Fatal(err)
	}
	if err := session.RestoreHosts(before, newRecord); err != nil {
		t.Fatal(err)
	}
	device.mu.Lock()
	defer device.mu.Unlock()
	if device.clears != 2 || device.writes != 2 || !bytes.Equal(device.table, before) {
		t.Fatalf("switch/return was not exact: clears=%d writes=%d", device.clears, device.writes)
	}
}

func TestHostWriteRefusesStaleOrMultipleEntryBackup(t *testing.T) {
	session, device := simulatedHosts(t, hostTable(9))
	if err := session.ReplaceHost(hostTable(1), hostTable(2)[8:]); err == nil {
		t.Fatal("stale table overwritten")
	}
	multi := make([]byte, 40)
	multi[0] = 2
	if err := session.ReplaceHost(multi, hostTable(2)[8:]); err == nil {
		t.Fatal("multi-entry table accepted")
	}
	device.mu.Lock()
	defer device.mu.Unlock()
	if device.clears != 0 || device.writes != 0 {
		t.Fatal("refused operation sent a write")
	}
}

func TestHostRecoveryHandlesClearedTableAfterBadAcknowledgement(t *testing.T) {
	before := hostTable(1)
	session, device := simulatedHosts(t, before)
	device.clearAckFailure = true
	if err := session.ReplaceHost(before, hostTable(2)[8:]); err == nil {
		t.Fatal("bad operation result accepted")
	}
	if err := session.RestoreHosts(before, hostTable(2)[8:]); err != nil {
		t.Fatal(err)
	}
	device.mu.Lock()
	defer device.mu.Unlock()
	if !bytes.Equal(device.table, before) || device.clears != 1 {
		t.Fatal("empty-table recovery failed")
	}
}

func TestHostTableLengthIsBounded(t *testing.T) {
	session, device := simulatedHosts(t, hostTable(1))
	device.badLength = true
	if _, err := session.ReadHosts(); err == nil {
		t.Fatal("unbounded host table accepted")
	}
}

func TestCancellationInterruptsScannerWelcomeRead(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		io.Copy(io.Discard, conn)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	channel := NewDataChannel("127.0.0.1", uint16(listener.Addr().(*net.TCPAddr).Port), [8]byte{}).WithContext(ctx)
	start := time.Now()
	_, err = channel.request(make([]byte, 64))
	if err == nil || time.Since(start) > time.Second {
		t.Fatal("welcome read did not stop on context cancellation")
	}
	<-finished
}

func TestDiscoveryUsesAdvertisedDynamicReplyPort(t *testing.T) {
	udp, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: DiscoveryPort})
	if err != nil {
		t.Fatal(err)
	}
	finished := make(chan struct{})
	replyPorts := make(chan int, 8)
	go func() {
		defer close(finished)
		buf := make([]byte, 512)
		for {
			n, remote, err := udp.ReadFromUDP(buf)
			if err != nil {
				return
			}
			if n != 32 || string(buf[:4]) != "VENS" {
				continue
			}
			port := int(binary.BigEndian.Uint16(buf[22:24]))
			replyPorts <- port
			response := make([]byte, 144)
			copy(response, Magic[:])
			binary.BigEndian.PutUint16(response[8:], 16)
			copy(response[16:], []byte{127, 0, 0, 1})
			binary.BigEndian.PutUint32(response[20:], 53218)
			binary.BigEndian.PutUint32(response[24:], 53219)
			copy(response[104:], "ScanSnap iX1300  ")
			copy(response[132:], "TEST000001")
			udp.WriteToUDP(response, &net.UDPAddr{IP: remote.IP, Port: port})
		}
	}()
	defer func() { udp.Close(); <-finished }()
	info, err := FindScanner(context.Background(), DiscoveryOptions{ScannerIP: "127.0.0.1", Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	port := <-replyPorts
	if port == 0 || port == ClientDiscoveryPort || info.Serial != "TEST000001" || info.Name != "ScanSnap iX1300" {
		t.Fatal(fmt.Sprintf("discovery decoded wrong fields or used fixed port: %d", port))
	}
}
