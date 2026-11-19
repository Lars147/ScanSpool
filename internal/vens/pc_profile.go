package vens

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"

	"scanspool/profiles"
)

func knownPCProfile(p pcScanProfile) bool {
	var reference pcScanProfile
	if err := json.Unmarshal(profiles.IX1300(), &reference); err != nil {
		panic(err)
	}
	return bytes.Equal(p.E9, reference.E9) && bytes.Equal(p.D4, reference.D4)
}

// PCReferenceConfig describes settings encoded by the captured, public profile.
// The reference keeps the manufacturer's original multi-feed flags.
func PCReferenceConfig() ScanConfig {
	return ScanConfig{ColorMode: ColorColor, Quality: QualitySuperFine, Duplex: true, PaperSize: PaperAuto, CompressionArg: 0x0d, MultiFeed: true}
}

func ValidatePCScanConfig(data []byte, cfg ScanConfig) error {
	p, err := parsePCScanProfile(data)
	if err != nil {
		return err
	}
	_, err = buildPCProfile(p, cfg)
	return err
}

func ValidatePCParameters(cfg ScanConfig) error {
	if _, ok := QualityDPI[cfg.Quality]; !ok || cfg.ColorMode < ColorAuto || cfg.ColorMode > ColorBW || cfg.BWDensity < -5 || cfg.BWDensity > 5 || (cfg.CompressionArg != 0 && (cfg.CompressionArg < 9 || cfg.CompressionArg > 13)) {
		return fmt.Errorf("invalid PC scan parameters")
	}
	if _, ok := PaperDimensions[cfg.PaperSize]; !ok {
		return fmt.Errorf("invalid paper size")
	}
	if cfg.ColorMode == ColorAuto && cfg.Quality != QualityAuto {
		return fmt.Errorf("automatic colour requires automatic resolution")
	}
	if cfg.BWDensity != 0 && cfg.ColorMode != ColorBW {
		return fmt.Errorf("density requires black-and-white mode")
	}
	maximum := PaperDimensions[PaperAuto]
	if cfg.PaperWidth > maximum.Width || cfg.PaperHeight > maximum.Height {
		return fmt.Errorf("custom scan area exceeds %d × %d (1/1200 inch)", maximum.Width, maximum.Height)
	}
	return nil
}

func buildPCProfile(p pcScanProfile, cfg ScanConfig) (pcScanProfile, error) {
	if err := ValidatePCParameters(cfg); err != nil {
		return p, err
	}
	if !knownPCProfile(p) {
		return p, fmt.Errorf("custom PC profile has no verified parameter mapping; use the bundled reference profile")
	}
	reference := PCReferenceConfig()
	if cfg == reference {
		return p, nil
	}
	// Parameter offsets follow the inspected manufacturer's SetParams and
	// SetPreReadMode serializers. Flag values inherited from the legacy path are
	// experimental until separately tested with this PC protocol on hardware.
	p.E9 = append([]byte(nil), p.E9...)
	p.D4 = append([]byte(nil), p.D4...)
	dpi := QualityDPI[cfg.Quality]
	for _, off := range []int{0, 2} {
		binary.BigEndian.PutUint16(p.E9[off:off+2], uint16(dpi))
	}
	dim := PaperDimensions[cfg.PaperSize]
	override := cfg.PaperSize != PaperAuto || cfg.PaperWidth > 0 || cfg.PaperHeight > 0
	if cfg.PaperWidth > 0 {
		dim.Width = cfg.PaperWidth
	}
	if cfg.PaperHeight > 0 {
		dim.Height = cfg.PaperHeight
	}
	if !override {
		dim = PaperDimension{0x2880, 0x4350}
	}
	binary.BigEndian.PutUint32(p.E9[4:8], uint32(dim.Width))
	binary.BigEndian.PutUint32(p.E9[8:12], uint32(dim.Height))
	mode := byte(5)
	if cfg.ColorMode == ColorGray {
		mode = 2
	}
	if cfg.ColorMode == ColorBW {
		mode = 0
	}
	p.E9[12] = mode
	legacy := MarshalScanConfig([8]byte{}, cfg)[64:]
	copyFields := func(offsets ...int) {
		for _, off := range offsets {
			p.D4[off] = legacy[off]
		}
	}
	if cfg.Duplex != reference.Duplex {
		copyFields(1)
	}
	if cfg.MultiFeed != reference.MultiFeed {
		copyFields(4, 6)
	}
	if cfg.ColorMode != reference.ColorMode {
		copyFields(7, 33, 38, 39, 40, 57, 60)
	}
	if cfg.Quality != reference.Quality {
		copyFields(7, 10, 34, 35, 36, 37)
	}
	if cfg.BlankPageRemoval != reference.BlankPageRemoval {
		copyFields(8)
	}
	if cfg.BleedThrough != reference.BleedThrough {
		copyFields(11)
	}
	if cfg.CompressionArg != reference.CompressionArg {
		copyFields(40)
	}
	if cfg.BWDensity != reference.BWDensity {
		copyFields(60)
	}

	if override {
		p.D4[2], p.D4[3], p.D4[5] = 0, 0, 0
		binary.BigEndian.PutUint16(p.D4[44:46], dim.Width)
		binary.BigEndian.PutUint16(p.D4[48:50], dim.Height)
	}
	// Automatic colour+quality uses a second window in the manufacturer serializer.
	if len(legacy) == 128 {
		p.D4 = append(p.D4, legacy[80:]...)
		binary.BigEndian.PutUint16(p.D4[92:94], dim.Width)
		binary.BigEndian.PutUint16(p.D4[96:98], dim.Height)
	}
	return p, nil
}
