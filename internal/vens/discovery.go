package vens

import (
	"context"
	"crypto/rand"
	"fmt"
	"log/slog"
	"net"
	"sort"
	"time"
)

// NewToken generates an 8-byte session token (6 random + 2 null).
func NewToken() [8]byte {
	var token [8]byte
	rand.Read(token[:6])
	slog.Debug("generated session token", "token", fmt.Sprintf("%x", token))
	return token
}

// GetLocalIP returns the local IP used to reach the given target.
// It dials a UDP socket to the target to let the OS routing table
// pick the correct outbound interface. If targetIP is empty,
// the link-local all-hosts multicast address (224.0.0.1) is used
// to determine the default LAN interface without any external dependency.
func GetLocalIP(targetIP string) string {
	if targetIP == "" {
		targetIP = "224.0.0.1"
	}
	conn, err := net.Dial("udp4", net.JoinHostPort(targetIP, "80"))
	if err != nil {
		return "0.0.0.0"
	}
	defer conn.Close()
	addr := conn.LocalAddr().(*net.UDPAddr)
	return addr.IP.String()
}

// DiscoveryOptions configures scanner discovery.
type DiscoveryOptions struct {
	ScannerIP string // Empty for broadcast discovery
	Token     [8]byte
	Timeout   time.Duration
}

// FindScanner returns the first matching scanner, using an ephemeral reply port.
func FindScanner(ctx context.Context, opts DiscoveryOptions) (*DeviceInfo, error) {
	found, err := discover(ctx, opts, false)
	if err != nil {
		return nil, err
	}
	if len(found) == 0 {
		return nil, fmt.Errorf("no scanner found")
	}
	return &found[0], nil
}

// FindScanners collects all scanners during a bounded discovery window.
func FindScanners(ctx context.Context, opts DiscoveryOptions) ([]DeviceInfo, error) {
	return discover(ctx, opts, true)
}

func discover(ctx context.Context, opts DiscoveryOptions, collect bool) ([]DeviceInfo, error) {
	if opts.Timeout == 0 {
		opts.Timeout = 5 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{Port: 0})
	if err != nil {
		return nil, fmt.Errorf("bind discovery: %w", err)
	}
	defer conn.Close()
	port := uint16(conn.LocalAddr().(*net.UDPAddr).Port)
	type target struct{ local, remote string }
	var targets []target
	if opts.ScannerIP != "" {
		if err := ValidateScannerIP(opts.ScannerIP); err != nil {
			return nil, err
		}
		targets = append(targets, target{GetLocalIP(opts.ScannerIP), opts.ScannerIP})
	} else {
		interfaces, err := net.Interfaces()
		if err != nil {
			return nil, err
		}
		for _, iface := range interfaces {
			if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
				continue
			}
			addresses, err := iface.Addrs()
			if err != nil {
				return nil, err
			}
			for _, address := range addresses {
				network, ok := address.(*net.IPNet)
				if !ok || network.IP.To4() == nil || network.IP.IsLoopback() {
					continue
				}
				ip := network.IP.To4()
				broadcast := make(net.IP, 4)
				for i := range 4 {
					broadcast[i] = ip[i] | ^network.Mask[i]
				}
				targets = append(targets, target{ip.String(), broadcast.String()})
			}
		}
	}
	if len(targets) == 0 {
		return nil, fmt.Errorf("no IPv4 network interface available")
	}
	send := func() error {
		var lastErr error
		sent := false
		for _, t := range targets {
			addr := &net.UDPAddr{IP: net.ParseIP(t.remote), Port: DiscoveryPort}
			for _, packet := range [][]byte{MarshalDiscoveryVENS(t.local, opts.Token, port, false), MarshalDiscoverySSNR(t.local, opts.Token, port)} {
				if _, err := conn.WriteToUDP(packet, addr); err != nil {
					lastErr = err
				} else {
					sent = true
				}
			}
		}
		if !sent {
			return lastErr
		}
		return nil
	}
	if err := send(); err != nil {
		return nil, fmt.Errorf("send discovery: %w", err)
	}
	found := map[string]DeviceInfo{}
	results := func() []DeviceInfo {
		out := make([]DeviceInfo, 0, len(found))
		for _, info := range found {
			out = append(out, info)
		}
		sort.Slice(out, func(i, j int) bool { return out[i].DeviceIP < out[j].DeviceIP })
		return out
	}
	buf := make([]byte, 512)
	for {
		if ctx.Err() != nil {
			if collect && ctx.Err() == context.DeadlineExceeded {
				return results(), nil
			}
			if len(found) > 0 {
				return results(), nil
			}
			return nil, ctx.Err()
		}
		conn.SetReadDeadline(time.Now().Add(250 * time.Millisecond))
		n, remote, err := conn.ReadFromUDP(buf)
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				if ctx.Err() == nil {
					if err := send(); err != nil {
						return nil, err
					}
				}
				continue
			}
			return nil, fmt.Errorf("read discovery: %w", err)
		}
		if opts.ScannerIP != "" && !remote.IP.Equal(net.ParseIP(opts.ScannerIP)) {
			continue
		}
		info, err := ParseDeviceInfo(buf[:n])
		if err != nil || !remote.IP.Equal(net.ParseIP(info.DeviceIP)) || info.DataPort == 0 || info.ControlPort == 0 {
			continue
		}
		found[info.DeviceIP] = *info
		if !collect {
			return results(), nil
		}
	}
}
