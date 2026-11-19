package vens

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"time"
)

// PCReserveTemplate reconstructs the verified iX1300 interface-16 handshake.
// Transport, token and clock fields are filled when reserving a session.
func PCReserveTemplate(info DeviceInfo, hostID uint64, password string) ([]byte, error) {
	if info.Name != "ScanSnap iX1300" || info.InterfaceVersion != 16 || hostID == 0 {
		return nil, fmt.Errorf("PC setup supports ScanSnap iX1300 interface 16 with a nonzero host ID")
	}
	p := newPacket(128)
	p.putU32(0, 128)
	p.putBytes(4, Magic[:])
	p.putU32(8, CmdReserve)
	p.putU32(32, 0x00101e00)
	if info.PasswordRequired {
		if len(password) == 0 || len(password) > 16 {
			return nil, fmt.Errorf("scanner connection password must contain 1 to 16 ASCII characters")
		}
		p.putU32(40, 1)
		for i := range len(password) {
			if password[i] < 0x20 || password[i] > 0x7e {
				return nil, fmt.Errorf("scanner connection password must contain printable ASCII characters")
			}
			copy(p[52+3*i:], fmt.Sprintf("%03d", int(password[i])+int(identityKey[i])+identityShift))
		}
	}
	binary.BigEndian.PutUint64(p[108:116], hostID)
	return p, nil
}

// PCHostSession is a bounded setup session. Close releases it normally; it does
// not remove a persistent host registration. The caller must persist a backup
// before invoking ReplaceHost.
type PCHostSession struct {
	control *ControlSession
	data    *DataChannel
	token   [8]byte
	hb      *Heartbeat
	closed  bool
}

func OpenPCHostSession(ctx context.Context, info DeviceInfo, template []byte, mode uint32) (*PCHostSession, error) {
	if mode != 0 && mode != 2 {
		return nil, fmt.Errorf("unsupported PC reserve mode")
	}
	if info.Paired || (info.ClientIP != "" && info.ClientIP != "0.0.0.0") {
		return nil, fmt.Errorf("scanner is in use; disconnect the other scanner client first")
	}
	token := NewToken()
	// Announce this session's token before opening a TCP reservation.
	announced, err := FindScanner(ctx, DiscoveryOptions{ScannerIP: info.DeviceIP, Token: token, Timeout: 5 * time.Second})
	if err != nil {
		return nil, err
	}
	if announced.Serial != info.Serial || announced.Name != info.Name {
		return nil, fmt.Errorf("scanner identity changed during setup")
	}
	if announced.Paired || (announced.ClientIP != "" && announced.ClientIP != "0.0.0.0") {
		return nil, fmt.Errorf("scanner became occupied; no reservation sent")
	}
	control := NewControlSession(info.DeviceIP, announced.ControlPort).WithContext(ctx)
	req, err := MarshalPCReserveFromTemplate(template, token, GetLocalIP(info.DeviceIP), ClientNotifyPort, time.Now())
	if err != nil {
		return nil, err
	}
	binary.BigEndian.PutUint32(req[36:40], mode)
	hb, err := StartHeartbeat(ctx, info.DeviceIP, token, 0)
	if err != nil {
		return nil, err
	}
	select {
	case <-ctx.Done():
		hb.Stop()
		return nil, ctx.Err()
	case <-time.After(300 * time.Millisecond):
	}
	response, err := control.sendRecv(req)
	if err != nil {
		hb.Stop()
		return nil, err
	}
	status, err := ParseReserveResponse(response)
	if err != nil || status != 0 {
		hb.Stop()
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("scanner reserve rejected (status %d)", int32(status))
	}
	return &PCHostSession{control: control, data: NewDataChannel(info.DeviceIP, announced.DataPort, token).WithContext(ctx), token: token, hb: hb}, nil
}

func (s *PCHostSession) Close() error {
	if s.closed {
		return nil
	}
	s.closed = true
	defer s.hb.Stop()
	return s.control.Deregister(s.token)
}

func (s *PCHostSession) hostCommand(op byte, read uint32, out []byte) ([]byte, error) {
	cdb := []byte{0xdc, 0, op, 0, byte(len(out) >> 8), byte(len(out)), byte(read >> 8), byte(read)}
	response, err := s.data.request(marshalPCSCSI(s.token, cdb, read, out))
	if err != nil {
		return nil, err
	}
	if len(response) < 40 || !bytes.Equal(response[4:8], Magic[:]) || binary.BigEndian.Uint32(response[12:16]) != 0 {
		return nil, fmt.Errorf("host operation %d failed at transport level", op)
	}
	return response[40:], nil
}

func (s *PCHostSession) ReadHosts() ([]byte, error) {
	lengthData, err := s.hostCommand(4, 4, nil)
	if err != nil {
		return nil, err
	}
	if len(lengthData) != 4 {
		return nil, fmt.Errorf("unexpected host-table length reply")
	}
	length := binary.BigEndian.Uint16(lengthData)
	if length < 8 || length > 504 || (length-8)%16 != 0 {
		return nil, fmt.Errorf("unrecognized host-table length")
	}
	out := make([]byte, 2)
	binary.BigEndian.PutUint16(out, length)
	table, err := s.hostCommand(1, uint32(length), out)
	if err != nil {
		return nil, err
	}
	if len(table) != int(length) || len(table) != 8+int(table[0])*16 {
		return nil, fmt.Errorf("host-table count does not match its length")
	}
	return table, nil
}

func (s *PCHostSession) result(op byte, out []byte) error {
	result, err := s.hostCommand(op, 2, out)
	if err != nil {
		return err
	}
	if len(result) != 2 || result[0] != 0 {
		return fmt.Errorf("host operation %d rejected (result %x)", op, result)
	}
	return nil
}

// ReplaceHost changes only an empty or single-entry table that still exactly
// matches the caller's persisted backup. It never clears multiple host entries.
func (s *PCHostSession) ReplaceHost(before []byte, record []byte) error {
	if len(record) != 16 || bytes.Equal(record[:8], make([]byte, 8)) ||
		len(before) < 8 || before[0] > 1 || len(before) != 8+int(before[0])*16 {
		return fmt.Errorf("expected an empty/single host table and valid host record")
	}
	current, err := s.ReadHosts()
	if err != nil {
		return err
	}
	if !bytes.Equal(current, before) {
		return fmt.Errorf("host table changed; registration stopped before writing")
	}
	if len(current) == 24 && bytes.Equal(current[8:], record) {
		return nil
	}
	if before[0] == 1 {
		if err := s.result(3, nil); err != nil {
			return err
		}
		empty, err := s.ReadHosts()
		if err != nil {
			return err
		}
		if empty[0] != 0 {
			return fmt.Errorf("host table was not empty after clear")
		}
	}
	if err := s.result(0, record); err != nil {
		return err
	}
	after, err := s.ReadHosts()
	if err != nil {
		return err
	}
	if len(after) != 24 || !bytes.Equal(after[8:], record) {
		return fmt.Errorf("host registration readback did not match")
	}
	return nil
}

func (s *PCHostSession) Inquiry() error {
	response, err := s.data.request(marshalPCSCSI(s.token, []byte{0x12, 0, 0, 0, 96, 0}, 96, nil))
	if err != nil {
		return err
	}
	if len(response) <= 40 || binary.BigEndian.Uint32(response[12:16]) != 0 {
		return fmt.Errorf("scanner inquiry failed")
	}
	return nil
}

// ValidateScannerIP limits setup to a single explicit IPv4 target when provided.
func ValidateScannerIP(ip string) error {
	parsed := net.ParseIP(ip).To4()
	if parsed == nil || parsed.IsUnspecified() || parsed.IsMulticast() || parsed.Equal(net.IPv4bcast) {
		return fmt.Errorf("enter a scanner IPv4 address")
	}
	return nil
}

// RestoreHosts restores a persisted empty/single-entry backup only if the
// current table is unchanged, empty, or contains precisely our candidate.
func (s *PCHostSession) RestoreHosts(before, candidate []byte) error {
	if len(before) < 8 || before[0] > 1 || len(before) != 8+int(before[0])*16 || len(candidate) != 16 {
		return fmt.Errorf("invalid host recovery data")
	}
	current, err := s.ReadHosts()
	if err != nil {
		return err
	}
	if bytes.Equal(current, before) {
		return nil
	}
	if current[0] != 0 && (len(current) != 24 || !bytes.Equal(current[8:], candidate)) {
		return fmt.Errorf("host table changed unexpectedly; recovery will not overwrite it")
	}
	if before[0] == 1 {
		if err := s.ReplaceHost(current, before[8:]); err != nil {
			return err
		}
	} else if current[0] == 1 {
		if err := s.result(3, nil); err != nil {
			return err
		}
	}
	restored, err := s.ReadHosts()
	if err != nil {
		return err
	}
	if !bytes.Equal(restored, before) {
		return fmt.Errorf("restored host table differs from its backup")
	}
	return nil
}
