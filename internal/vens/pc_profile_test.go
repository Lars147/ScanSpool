package vens

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"testing"

	"scanspool/profiles"
)

func TestReferencePCProfileIsByteExact(t *testing.T) {
	p, err := parsePCScanProfile(profiles.IX1300())
	if err != nil {
		t.Fatal(err)
	}
	got, err := buildPCProfile(p, PCReferenceConfig())
	if err != nil || !bytes.Equal(got.E9, p.E9) || !bytes.Equal(got.D4, p.D4) {
		t.Fatal("reference changed", err)
	}
	p.D4[62] ^= 1
	if _, err := buildPCProfile(p, PCReferenceConfig()); err == nil {
		t.Fatal("unknown custom profile modified")
	}
	raw, _ := json.Marshal(p)
	if ValidatePCScanProfile(raw) == nil {
		t.Fatal("unknown custom profile activated")
	}
}
func TestExperimentalPCProfileMapsExistingParameters(t *testing.T) {
	p, _ := parsePCScanProfile(profiles.IX1300())
	cfg := PCReferenceConfig()
	cfg.Quality = QualityFine
	cfg.ColorMode = ColorGray
	cfg.Duplex = false
	cfg.CompressionArg = 0x09
	cfg.PaperSize = PaperA4
	cfg.BlankPageRemoval = true
	cfg.BleedThrough = true
	cfg.MultiFeed = false
	got, err := buildPCProfile(p, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if binary.BigEndian.Uint16(got.E9) != 200 || got.E9[12] != 2 || binary.BigEndian.Uint16(got.D4[34:36]) != 200 || got.D4[1] != 1 || got.D4[38] != 2 || got.D4[40] != 9 {
		t.Fatal("image parameters not applied")
	}
	if got.D4[8] != 0xe0 || got.D4[11] != 0xc0 || got.D4[6] != 0xc0 || got.D4[2] != 0 || binary.BigEndian.Uint16(got.D4[44:46]) != PaperDimensions[PaperA4].Width {
		t.Fatal("paper/correction parameters not applied")
	}
	for _, i := range []int{0, 9, 12, 31, 41, 50, 62, 79} {
		if got.D4[i] != p.D4[i] {
			t.Fatalf("unknown byte %d changed", i)
		}
	}
	if p.D4[1] != 3 {
		t.Fatal("template mutated")
	}
	cfg.ColorMode = ColorBW
	cfg.BWDensity = -3
	got, err = buildPCProfile(p, cfg)
	if err != nil || got.D4[38] != 0 || got.D4[60] != 3 {
		t.Fatal("BW density", err)
	}
	cfg.ColorMode = ColorAuto
	cfg.Quality = QualityAuto
	cfg.Duplex = true
	cfg.BWDensity = 0
	got, err = buildPCProfile(p, cfg)
	if err != nil || len(got.D4) != 128 || got.D4[7] != 0xc1 {
		t.Fatal("auto window", err)
	}
	cfg.ColorMode = ColorColor
	cfg.PaperWidth = 1000
	cfg.PaperHeight = 2000
	got, err = buildPCProfile(p, cfg)
	if err != nil || binary.BigEndian.Uint32(got.E9[4:8]) != 1000 || binary.BigEndian.Uint16(got.D4[48:50]) != 2000 {
		t.Fatal("explicit scan region", err)
	}
}
