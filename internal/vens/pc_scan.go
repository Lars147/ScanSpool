package vens

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"time"
)

type pcScanProfile struct {
	E9 []byte `json:"e9"`
	D4 []byte `json:"d4"`
}

// marshalPCSCSI carries only a SCSI command and optional scan parameters.
func marshalPCSCSI(token [8]byte, cdb []byte, readLength uint32, out []byte) []byte {
	p := make([]byte, 64+len(out))
	binary.BigEndian.PutUint32(p[:4], uint32(len(p)))
	copy(p[4:8], Magic[:])
	binary.BigEndian.PutUint32(p[8:12], 1)
	copy(p[16:24], token[:])
	binary.BigEndian.PutUint32(p[32:36], uint32(len(cdb)))
	binary.BigEndian.PutUint32(p[36:40], readLength)
	binary.BigEndian.PutUint32(p[40:44], uint32(len(out)))
	copy(p[48:64], cdb)
	copy(p[64:], out)
	return p
}

func pcExchange(conn net.Conn, request []byte) ([]byte, error) {
	conn.SetDeadline(time.Now().Add(15 * time.Second))
	if _, err := conn.Write(request); err != nil {
		return nil, err
	}
	return readResponse(conn)
}

func pcCommand(conn net.Conn, request []byte) error {
	response, err := pcExchange(conn, request)
	if err != nil {
		return err
	}
	return validatePCResponse(response, request[48])
}

func validatePCResponse(response []byte, opcode byte) error {
	if len(response) < 16 || !bytes.Equal(response[4:8], Magic[:]) {
		return fmt.Errorf("invalid PC command %02x response", opcode)
	}
	if status := binary.BigEndian.Uint32(response[12:16]); status != 0 {
		return fmt.Errorf("PC SCSI command %02x returned status %d", opcode, status)
	}
	return nil
}

func parsePCSense(response []byte) error {
	if len(response) < SenseDataOffset+14 || !bytes.Equal(response[4:8], Magic[:]) {
		return fmt.Errorf("invalid PC SENSE response")
	}
	if err := parseSenseError(response); err != nil {
		return err
	}
	// CHECK CONDITION is also used for the vendor's normal scan-complete sense.
	status := binary.BigEndian.Uint32(response[12:16])
	if status != 0 && status != 2 {
		return fmt.Errorf("PC SENSE status %d", status)
	}
	return nil
}

func (d *DataChannel) startPCScan(profilePath string, cfg ScanConfig) (*ScanSession, error) {
	data, err := os.ReadFile(profilePath)
	if err != nil {
		return nil, err
	}
	return d.startPCScanData(data, cfg)
}

// ValidatePCScanProfile fails before activation rather than on the first paper feed.
func ValidatePCScanProfile(data []byte) error {
	p, err := parsePCScanProfile(data)
	if err != nil {
		return err
	}
	if !knownPCProfile(p) {
		return fmt.Errorf("custom PC profile has no verified parameter mapping; use the bundled reference profile")
	}
	return nil
}
func parsePCScanProfile(data []byte) (pcScanProfile, error) {
	var profile pcScanProfile
	if err := json.Unmarshal(data, &profile); err != nil {
		return profile, err
	}
	if len(profile.E9) != 32 || (len(profile.D4) != 80 && len(profile.D4) != 128) || (profile.D4[1] != 1 && profile.D4[1] != 3) {
		return profile, fmt.Errorf("unrecognized observed iX1300 scan profile")
	}
	return profile, nil
}
func (d *DataChannel) startPCScanData(data []byte, cfg ScanConfig) (_ *ScanSession, scanErr error) {
	profile, err := parsePCScanProfile(data)
	if err != nil {
		return nil, err
	}
	profile, err = buildPCProfile(profile, cfg)
	if err != nil {
		return nil, err
	}
	session := &ScanSession{dc: d, token: d.token, sidesPerSheet: 2, pcMode: true, onClose: d.onClose}
	if !cfg.Duplex {
		session.sidesPerSheet = 1
	}
	session.scanConfig = cfg
	started := false
	defer func() {
		if !started {
			scanErr = errors.Join(scanErr, session.Close())
		}
	}()
	if err := session.preparePCScan(); err != nil {
		return nil, fmt.Errorf("PC prepare: %w", err)
	}
	conn, err := d.connect()
	if err != nil {
		return nil, err
	}
	session.conn = conn
	commands := [][]byte{
		marshalPCSCSI(d.token, []byte{0xd8, 0, 0, 0, 0, 0}, 0, nil),
		marshalPCSCSI(d.token, []byte{0xe9, 0, 0, 0, 0, 0, 0, 0x20, 0, 0}, 0, profile.E9),
		marshalPCSCSI(d.token, []byte{0xd4, 0, 0, 0, byte(len(profile.D4)), 0}, 0, profile.D4),
		MarshalGetPageMetadata(d.token),
		MarshalWaitForScan(d.token),
	}
	for _, command := range commands {
		slog.Info("PC scan setup", "opcode", fmt.Sprintf("%02x", command[48]))
		var err error
		if command[48] == 0x03 {
			var response []byte
			response, err = pcExchange(conn, command)
			if err == nil {
				err = parsePCSense(response)
			}
		} else {
			err = pcCommand(conn, command)
		}
		if err != nil {
			return nil, err
		}
	}
	started = true
	slog.Info("PC scan started", "dpi", QualityDPI[cfg.Quality], "duplex", cfg.Duplex)
	return session, nil
}

func (d *DataChannel) pcADFStatus() (*ADFStatus, error) {
	response, err := d.request(marshalPCSCSI(d.token, []byte{0xc2, 0, 0, 0, 0, 0, 0, 0, 0x30, 0}, 0x30, nil))
	if err != nil {
		return nil, err
	}
	if err := validatePCResponse(response, 0xc2); err != nil {
		return nil, err
	}
	if len(response) < 88 {
		return nil, fmt.Errorf("short PC hardware status: %d bytes", len(response))
	}
	return &ADFStatus{HasPaper: response[43]&0x80 == 0}, nil
}

func (s *ScanSession) nextPCPage() (Page, error) {
	if s.done {
		return Page{}, io.EOF
	}
	if s.sideIdx == 0 && s.physicalSheet > 0 {
		status, err := s.dc.pcADFStatus()
		if err != nil {
			return Page{}, err
		}
		if !status.HasPaper {
			// The observed vendor sequence ends a batch with E0 then SENSE.
			if response, err := pcExchange(s.conn, MarshalWaitForScan(s.token)); err != nil {
				return Page{}, err
			} else if len(response) < 16 || !bytes.Equal(response[4:8], Magic[:]) || (binary.BigEndian.Uint32(response[12:16]) != 0 && binary.BigEndian.Uint32(response[12:16]) != 2) {
				return Page{}, fmt.Errorf("invalid terminal PC wait response")
			}
			sense, err := pcExchange(s.conn, MarshalGetPageMetadata(s.token))
			if err != nil {
				return Page{}, err
			}
			if err := parsePCSense(sense); err != nil {
				return Page{}, err
			}
			if err := s.Close(); err != nil {
				return Page{}, err
			}
			s.done = true
			return Page{}, io.EOF
		}
		if err := pcCommand(s.conn, MarshalWaitForScan(s.token)); err != nil {
			return Page{}, err
		}
	}
	if s.transferSheet >= 256 {
		return Page{}, fmt.Errorf("PC page identifier exhausted")
	}
	limit := uint32(0x300000)
	if s.scanConfig != PCReferenceConfig() {
		limit = 0xc00000
	}
	back := s.sideIdx == 1
	cdb := []byte{0x28, 0, 0, 2, 0, 0, 0x30, 0, 0, 0, byte(s.transferSheet), 0}
	if back {
		cdb[5] = 0x80
	}
	cdb[6], cdb[7], cdb[8] = byte(limit>>16), byte(limit>>8), byte(limit)
	request := marshalPCSCSI(s.token, cdb, limit, nil)
	s.conn.SetDeadline(time.Now().Add(30 * time.Second))
	if _, err := s.conn.Write(request); err != nil {
		return Page{}, err
	}
	var image []byte
	prefix := -1
	complete := false
	for frames := 0; frames < 256; frames++ {
		frame, err := readResponse(s.conn)
		if err != nil {
			return Page{}, err
		}
		if len(frame) < 42 || len(frame) > int(limit)+128 {
			return Page{}, fmt.Errorf("unexpected PC image frame length: %d", len(frame))
		}
		if prefix < 0 {
			for start := 40; start <= 48 && start+4 <= len(frame); start++ {
				payload := frame[start:]
				if bytes.HasPrefix(payload, []byte{0xff, 0xd8, 0xff}) || bytes.HasPrefix(payload, []byte{'I', 'I', 42, 0}) || bytes.HasPrefix(payload, []byte{'M', 'M', 0, 42}) {
					prefix = start
					break
				}
			}
			if prefix < 0 {
				return Page{}, fmt.Errorf("PC image frame lacks a supported JPEG or TIFF header")
			}
		}
		if len(frame) < prefix {
			return Page{}, fmt.Errorf("short PC image continuation")
		}
		image = append(image, frame[prefix:]...)
		if len(image) > int(limit) {
			return Page{}, fmt.Errorf("PC image exceeds requested allocation")
		}
		var end int
		if bytes.HasPrefix(image, []byte{0xff, 0xd8}) {
			end, err = jpegEnd(image)
		} else {
			end, err = tiffEnd(image)
		}
		if err != nil {
			return Page{}, err
		}
		if end > 0 {
			image = image[:end]
			complete = true
			break
		}
	}
	if !complete || len(image) == 0 {
		return Page{}, fmt.Errorf("PC image did not finish")
	}
	if _, _, err := (Page{JPEG: image}).ImageInfo(); err != nil {
		return Page{}, err
	}
	sense, err := pcExchange(s.conn, MarshalGetPageMetadata(s.token))
	if err != nil {
		return Page{}, err
	}
	if err := parsePCSense(sense); err != nil {
		return Page{}, err
	}
	page := Page{Sheet: s.physicalSheet, Side: s.sideIdx, JPEG: image}
	actualDPI := ImageDPI(image)
	if actualDPI == 0 {
		actualDPI = QualityDPI[s.scanConfig.Quality]
	}
	if actualDPI == 0 {
		return Page{}, fmt.Errorf("automatic PC resolution requires actual per-page DPI metadata")
	}
	info, format, err := page.ImageInfo()
	if err != nil {
		return Page{}, err
	}
	page.Format = format
	page.ColorMode, _ = page.ImageColorMode()
	page.PixelSize = &PixelSizeInfo{XPixels: info.Width, YPixels: info.Height, XRes: actualDPI, YRes: actualDPI}
	s.transferSheet++
	s.sideIdx++
	if s.sideIdx >= s.sidesPerSheet {
		s.sideIdx = 0
		s.physicalSheet++
	}
	slog.Info("PC page received", "side", page.Side, "bytes", len(image))
	return page, nil
}

func (s *ScanSession) preparePCScan() error {
	conn, err := s.dc.connect()
	if err != nil {
		return err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	// Once a write is attempted, a lost response cannot prove D5 was not applied.
	s.pcPrepared = true
	if _, err := conn.Write(MarshalPrepareScan(s.token)); err != nil {
		return err
	}
	response, err := readResponse(conn)
	if err != nil {
		return err
	}
	return validatePCResponse(response, 0xd5)
}
