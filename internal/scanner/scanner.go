package scanner

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"scanspool/internal/vens"
)

// Scanner is a high-level interface for ScanSnap operations.
type Scanner struct {
	mu               sync.Mutex
	host             string
	dataPort         uint16
	controlPort      uint16
	token            [8]byte
	identity         string
	control          *vens.ControlSession
	heartbeat        *vens.Heartbeat
	connected        bool
	reserved         bool
	name             string
	serial           string
	deviceName       string           // full device name with manufacturer from TCP GET_SET sub=0x12
	firmwareRevision string           // firmware revision from device name suffix (e.g. "0M00")
	scanParams       *vens.ScanParams // capabilities from INQUIRY VPD 0xF0
	wifiState        uint32           // last GET_WIFI_STATUS state (signal strength, 0 to 3)

	reserveTemplate []byte
	pcProfile       []byte
	expectedSerial  string

	busyCheck           func() bool
	operationGuard      *ScanGuard
	connectionContext   context.Context
	lastConnectionError string
	reconnCancel        context.CancelFunc
	reconnDone          chan struct{}
}

// Option customizes a scanner instance without process-wide environment changes.
type Option func(*Scanner)

func WithPCConfig(template, profile []byte) Option {
	return func(s *Scanner) {
		s.reserveTemplate = append([]byte(nil), template...)
		s.pcProfile = append([]byte(nil), profile...)
	}
}

func WithScanGuard(guard *ScanGuard) Option { return func(s *Scanner) { s.operationGuard = guard } }

func WithBusyCheck(check func() bool) Option { return func(s *Scanner) { s.busyCheck = check } }

func WithSerial(serial string) Option { return func(s *Scanner) { s.expectedSerial = serial } }

// New creates a Scanner targeting the given host with a pre-computed identity.
func New(host string, dataPort, controlPort uint16, identity string, options ...Option) *Scanner {
	var token [8]byte
	rand.Read(token[:6])
	slog.Debug("scanner created", "host", host, "dataPort", dataPort, "controlPort", controlPort, "token", fmt.Sprintf("%x", token))
	s := &Scanner{
		host:        host,
		dataPort:    dataPort,
		controlPort: controlPort,
		token:       token,
		identity:    identity,
	}
	for _, option := range options {
		option(s)
	}
	s.control = vens.NewControlSession(host, controlPort, s.reserveTemplate)
	return s
}

// Online returns whether the scanner session is active (thread-safe).
func (s *Scanner) Online() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.connected
}

// Connect establishes a session with the scanner: discovery, heartbeat, configure, data setup.
func (s *Scanner) Connect(ctx context.Context) (connectErr error) {
	s.mu.Lock()
	reserved := s.reserved
	s.mu.Unlock()
	if reserved {
		s.Disconnect()
	}
	// Clean up any previous connection state (idempotent for reconnection)
	s.mu.Lock()
	if s.heartbeat != nil {
		s.heartbeat.Stop()
		s.heartbeat = nil
	}
	s.connected = false
	s.mu.Unlock()

	// Step 1: UDP discovery to let the scanner know our token
	slog.Debug("discovery...", "host", s.host)
	info, err := vens.FindScanner(ctx, vens.DiscoveryOptions{
		ScannerIP: s.host,
		Token:     s.token,
	})
	if err != nil {
		return fmt.Errorf("discovery: %w", err)
	}
	slog.Debug("discovery OK", "name", info.Name, "serial", info.Serial, "ip", info.DeviceIP, "dataPort", info.DataPort, "controlPort", info.ControlPort)

	if s.expectedSerial != "" && info.Serial != s.expectedSerial {
		return fmt.Errorf("scanner identity does not match saved configuration")
	}

	// Update ports from discovery response
	s.mu.Lock()
	s.connectionContext = ctx
	if info.DataPort != 0 {
		s.dataPort = info.DataPort
	}
	if info.ControlPort != 0 {
		s.controlPort = info.ControlPort
		s.control = vens.NewControlSession(s.host, s.controlPort, s.reserveTemplate).WithContext(ctx)
	}

	s.mu.Unlock()

	if s.identity == "" && len(s.reserveTemplate) == 0 {
		identity, err := vens.ComputeIdentity(vens.PasswordFromSerial(info.Serial))
		if err != nil {
			return err
		}
		s.identity = identity
	}

	// Step 2: Start heartbeats
	slog.Debug("starting heartbeat...")
	hb, err := vens.StartHeartbeat(ctx, s.host, s.token, 0)
	if err != nil {
		return fmt.Errorf("heartbeat: %w", err)
	}
	s.mu.Lock()
	s.heartbeat = hb
	s.mu.Unlock()
	select {
	case <-ctx.Done():
		hb.Stop()
		return ctx.Err()
	case <-time.After(300 * time.Millisecond):
	}

	// Step 3: Configure session
	slog.Debug("configuring session...")
	localIP := vens.GetLocalIP(s.host)
	accepted, err := s.control.Configure(s.token, localIP, vens.ClientNotifyPort, s.identity)
	if err != nil {
		hb.Stop()
		s.mu.Lock()
		s.heartbeat = nil
		s.mu.Unlock()
		return fmt.Errorf("configure: %w", err)
	}
	if !accepted {
		hb.Stop()
		s.mu.Lock()
		s.heartbeat = nil
		s.mu.Unlock()
		return fmt.Errorf("pairing rejected — wrong password/identity")
	}

	s.mu.Lock()
	s.reserved = true
	s.mu.Unlock()
	defer func() {
		if connectErr != nil {
			releaseErr := s.control.Deregister(s.token)
			hb.Stop()
			s.mu.Lock()
			s.heartbeat = nil
			s.reserved = false
			s.mu.Unlock()
			connectErr = errors.Join(connectErr, releaseErr)
		}
	}()

	// Step 4: Data channel setup (with status check interleaved, matching Python flow)
	slog.Debug("data channel setup...", "host", s.host, "port", s.dataPort)
	dataCh := s.newDataChannel()
	devInfo, err := dataCh.GetDeviceInfo()
	if err != nil {
		slog.Warn("get device info failed, retrying in 2s", "err", err)
		time.Sleep(2 * time.Second)
		devInfo, err = dataCh.GetDeviceInfo()
		if err != nil {
			hb.Stop()
			s.mu.Lock()
			s.heartbeat = nil
			s.mu.Unlock()
			return fmt.Errorf("device info: %w", err)
		}
	}

	// Step 5: Status check (between data channel operations, matching Python flow)
	slog.Debug("status check...")
	if wifiState, err := s.control.CheckStatus(s.token); err != nil {
		slog.Warn("status check failed", "err", err)
	} else {
		s.mu.Lock()
		s.wifiState = wifiState
		s.mu.Unlock()
	}

	scanParams, err := dataCh.GetScanParams()
	if err != nil {
		slog.Warn("get scan params failed", "err", err)
	}

	if _, err := dataCh.SetConfig(); err != nil {
		slog.Warn("set config failed", "err", err)
	}

	s.mu.Lock()
	s.connected = true
	s.name = info.Name
	s.serial = info.Serial
	if devInfo != nil {
		s.deviceName = devInfo.DeviceName
		s.firmwareRevision = devInfo.FirmwareRevision
	}
	s.scanParams = scanParams
	s.mu.Unlock()
	slog.Info("connected to scanner", "host", s.host, "name", info.Name, "serial", info.Serial, "deviceName", s.deviceName)
	return nil
}

// StartScan begins a lazy scan session. Pages are pulled one at a time via
// ScanSession.NextPage, allowing the client to stop after any page.
func (s *Scanner) StartScan(cfg vens.ScanConfig) (*vens.ScanSession, error) {
	if !s.Online() {
		return nil, fmt.Errorf("scanner not connected")
	}
	slog.Info("starting scan session", "colorMode", cfg.ColorMode, "quality", cfg.Quality, "duplex", cfg.Duplex, "paperSize", cfg.PaperSize)
	if err := s.ValidateScanConfig(cfg); err != nil {
		return nil, err
	}
	dataCh := s.newDataChannel()
	session, err := dataCh.StartScan(cfg)
	if err != nil {
		s.scanFailed(err)
	}
	return session, err
}

// Scan executes a scan with the given config and returns pages.
func (s *Scanner) Scan(cfg vens.ScanConfig, onPage func(vens.Page)) ([]vens.Page, error) {
	if !s.Online() {
		return nil, fmt.Errorf("scanner not connected")
	}
	slog.Info("starting scan", "colorMode", cfg.ColorMode, "quality", cfg.Quality, "duplex", cfg.Duplex, "paperSize", cfg.PaperSize)
	if err := s.ValidateScanConfig(cfg); err != nil {
		return nil, err
	}
	dataCh := s.newDataChannel()
	pages, err := dataCh.RunScan(cfg, onPage)
	if err != nil {
		slog.Warn("scan error", "err", err, "pages_so_far", len(pages))
		return pages, err
	}
	// Filter out empty pages
	var result []vens.Page
	for _, p := range pages {
		if len(p.JPEG) > 0 {
			result = append(result, p)
		}
	}
	slog.Info("scan complete", "total_pages", len(pages), "non_empty", len(result))
	return result, nil
}

// Disconnect deregisters from the scanner and stops heartbeat.
func (s *Scanner) Disconnect() {
	s.mu.Lock()
	reserved := s.reserved
	s.reserved = false
	s.mu.Unlock()
	slog.Debug("disconnecting from scanner...")
	if reserved && s.control != nil {
		if err := s.control.Deregister(s.token); err != nil {
			slog.Warn("deregister failed", "err", err)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.heartbeat != nil {
		s.heartbeat.Stop()
		s.heartbeat = nil
	}
	s.connected = false
	slog.Info("disconnected from scanner")
}

// markOffline stops the heartbeat and marks the scanner as disconnected.
func (s *Scanner) markOffline() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.connected {
		return
	}
	slog.Warn("scanner went offline", "host", s.host)
	if s.heartbeat != nil {
		s.heartbeat.Stop()
		s.heartbeat = nil
	}
	s.connected = false
}

// StartReconnectLoop starts a background goroutine that monitors scanner
// health and reconnects automatically when the connection is lost.
func (s *Scanner) StartReconnectLoop(ctx context.Context) {
	ctx, cancel := context.WithCancel(ctx)
	s.reconnCancel = cancel
	s.reconnDone = make(chan struct{})
	go s.reconnectLoop(ctx)
}

// StopReconnectLoop stops the reconnection goroutine and waits for it to exit.
func (s *Scanner) StopReconnectLoop() {
	if s.reconnCancel != nil {
		s.reconnCancel()
		<-s.reconnDone
	}
}

func (s *Scanner) reconnectLoop(ctx context.Context) {
	defer close(s.reconnDone)
	if !s.Online() && ctx.Err() == nil {
		s.backgroundCheck(ctx)
	}

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.backgroundCheck(ctx)
		}
	}
}

func (s *Scanner) backgroundCheck(ctx context.Context) {
	if s.operationGuard != nil {
		release, ok := s.operationGuard.TryAcquire()
		if !ok {
			return
		}
		defer release()
	} else if s.busyCheck != nil && s.busyCheck() {
		return
	}
	if ctx.Err() != nil {
		return
	}
	if s.Online() {
		s.healthCheck()
	} else {
		s.tryReconnect(ctx)
	}
}

func (s *Scanner) healthCheck() {
	s.mu.Lock()
	ctrl := s.control
	token := s.token
	s.mu.Unlock()

	if ctrl == nil {
		s.markOffline()
		return
	}
	state, err := ctrl.CheckStatus(token)
	if err != nil {
		slog.Warn("health check failed", "err", err)
		s.markOffline()
		return
	}
	s.mu.Lock()
	s.wifiState = state
	s.mu.Unlock()
}

func (s *Scanner) tryReconnect(ctx context.Context) {
	slog.Info("attempting reconnection...", "host", s.host)
	err := s.Connect(ctx)
	s.mu.Lock()
	if err != nil {
		s.lastConnectionError = err.Error()
	} else {
		s.lastConnectionError = ""
	}
	s.mu.Unlock()
	if err != nil {
		slog.Debug("reconnect failed", "host", s.host, "err", err)
	}
}

// CheckSenseStatus probes the scanner for error conditions via REQUEST SENSE.
func (s *Scanner) CheckSenseStatus() *vens.ScanError {
	if !s.Online() {
		return nil
	}
	dataCh := s.newDataChannel()
	return dataCh.CheckSenseStatus()
}

// CheckADFStatus queries the scanner's ADF and returns paper/error status.
func (s *Scanner) CheckADFStatus() (*vens.ADFStatus, error) {
	if !s.Online() {
		return nil, fmt.Errorf("scanner not connected")
	}
	dataCh := s.newDataChannel()
	return dataCh.CheckADFStatus()
}

// Host returns the scanner's IP address.
func (s *Scanner) Host() string { return s.host }

// Name returns the scanner's device name from discovery.
func (s *Scanner) Name() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.TrimSpace(s.name)
}

// Serial returns the scanner's serial number from discovery.
func (s *Scanner) Serial() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.serial
}

// DeviceName returns the full device name from TCP GET_SET sub=0x12
func (s *Scanner) DeviceName() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.deviceName
}

// FirmwareRevision returns the firmware revision from the device name suffix.
func (s *Scanner) FirmwareRevision() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.firmwareRevision
}

// ScanParams returns the scanner capabilities from INQUIRY VPD 0xF0.
func (s *Scanner) ScanParams() *vens.ScanParams {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.scanParams
}

// WifiState returns the last known WiFi state from GET_WIFI_STATUS (signal strength, 0 to 3).
func (s *Scanner) WifiState() uint32 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.wifiState
}

// MakeAndModel returns the device name (already stripped of firmware revision by ParseDataDeviceInfo).
// Falls back to the discovery name if the full device name is unavailable.
func (s *Scanner) MakeAndModel() string {
	s.mu.Lock()
	dn := s.deviceName
	name := strings.TrimSpace(s.name)
	s.mu.Unlock()
	if dn != "" {
		return dn
	}
	return name
}

func (s *Scanner) newDataChannel() *vens.DataChannel {
	s.mu.Lock()
	defer s.mu.Unlock()
	return vens.NewDataChannel(s.host, s.dataPort, s.token, s.pcProfile).WithContext(s.connectionContext).WithCloseObserver(s.scanFailed)
}

// PCMode identifies the currently supported fixed iX1300 hardware profile.
func (s *Scanner) PCMode() bool { return len(s.pcProfile) > 0 }

func (s *Scanner) ConnectionError() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastConnectionError
}

// A failed close leaves the device transaction uncertain. Reconnect owns the
// operation guard before it attempts to restore readiness.
func (s *Scanner) scanFailed(err error) {
	if err == nil {
		return
	}
	s.mu.Lock()
	s.connected = false
	s.lastConnectionError = err.Error()
	s.mu.Unlock()
}

func (s *Scanner) ValidateScanConfig(cfg vens.ScanConfig) error {
	if !s.PCMode() {
		return nil
	}
	return vens.ValidatePCScanConfig(s.pcProfile, cfg)
}
