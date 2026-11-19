package config

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"scanspool/internal/persist"
	"scanspool/internal/vens"
)

type ProfileView struct {
	Profile  vens.ScanConfig `json:"profile"`
	Revision string          `json:"revision"`
}

func profileView(profile vens.ScanConfig) ProfileView {
	data, _ := json.Marshal(profile)
	return ProfileView{Profile: profile, Revision: fmt.Sprintf("%x", sha256.Sum256(data))}
}

func (s *Store) Profile() ProfileView {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return profileView(s.profile)
}

func (s *Store) SaveProfile(update ProfileView) (ProfileView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if update.Revision != profileView(s.profile).Revision {
		return ProfileView{}, ErrConflict
	}
	if err := vens.ValidatePCParameters(update.Profile); err != nil {
		return ProfileView{}, err
	}
	data, err := json.MarshalIndent(update.Profile, "", "  ")
	if err != nil {
		return ProfileView{}, err
	}
	err = persist.WriteFile(profilePath(s.path), append(data, '\n'))
	if err == nil || persist.Committed(err) {
		s.profile = update.Profile
	}
	return profileView(s.profile), err
}

func profilePath(outputPath string) string {
	return filepath.Join(filepath.Dir(outputPath), "scan-profile.json")
}

// ScanOption gives the terminal and command line the same values and validation.
type ScanOption struct {
	Key, Label, Help string
	Choices          []string
	Value            func(vens.ScanConfig) string
	Set              func(*vens.ScanConfig, string) error
}

func scanChoice[T comparable](key, label string, names []string, values []T, member func(*vens.ScanConfig) *T) ScanOption {
	return ScanOption{
		Key: key, Label: label, Help: strings.Join(names, "|"), Choices: names,
		Value: func(p vens.ScanConfig) string {
			for i, value := range values {
				if *member(&p) == value {
					return names[i]
				}
			}
			return ""
		},
		Set: func(p *vens.ScanConfig, name string) error {
			for i, choice := range names {
				if name == choice {
					*member(p) = values[i]
					return nil
				}
			}
			return fmt.Errorf("%s: choose %s", label, strings.Join(names, ", "))
		},
	}
}

func scanToggle(key, label string, member func(*vens.ScanConfig) *bool) ScanOption {
	return scanChoice(key, label, []string{"off", "on"}, []bool{false, true}, member)
}

func scanNumber[T ~uint16 | ~int](key, label, help string, lower, upper int, member func(*vens.ScanConfig) *T) ScanOption {
	return ScanOption{
		Key: key, Label: label, Help: help,
		Value: func(p vens.ScanConfig) string { return strconv.Itoa(int(*member(&p))) },
		Set: func(p *vens.ScanConfig, text string) error {
			value, err := strconv.Atoi(strings.TrimSpace(text))
			if err != nil || value < lower || value > upper {
				return fmt.Errorf("%s: enter a number from %d to %d", label, lower, upper)
			}
			*member(p) = T(value)
			return nil
		},
	}
}

func ScanOptions() []ScanOption {
	maximum := vens.PaperDimensions[vens.PaperAuto]
	return []ScanOption{
		scanChoice("color", "Color mode", []string{"color", "gray", "bw", "auto"}, []vens.ColorMode{vens.ColorColor, vens.ColorGray, vens.ColorBW, vens.ColorAuto}, func(p *vens.ScanConfig) *vens.ColorMode { return &p.ColorMode }),
		scanChoice("dpi", "Resolution (dpi)", []string{"150", "200", "300", "auto"}, []vens.Quality{vens.QualityNormal, vens.QualityFine, vens.QualitySuperFine, vens.QualityAuto}, func(p *vens.ScanConfig) *vens.Quality { return &p.Quality }),
		scanChoice("sides", "Sides", []string{"simplex", "duplex"}, []bool{false, true}, func(p *vens.ScanConfig) *bool { return &p.Duplex }),
		scanChoice("paper", "Paper size", []string{"auto", "a4", "a5", "business-card", "postcard"}, []vens.PaperSize{vens.PaperAuto, vens.PaperA4, vens.PaperA5, vens.PaperBusinessCard, vens.PaperPostcard}, func(p *vens.ScanConfig) *vens.PaperSize { return &p.PaperSize }),
		scanNumber("width", "Width (1/1200 in, 0 = paper size)", "custom width in 1/1200 inch (0 = paper size)", 0, int(maximum.Width), func(p *vens.ScanConfig) *uint16 { return &p.PaperWidth }),
		scanNumber("height", "Height (1/1200 in, 0 = paper size)", "custom height in 1/1200 inch (0 = paper size)", 0, int(maximum.Height), func(p *vens.ScanConfig) *uint16 { return &p.PaperHeight }),
		scanChoice("compression", "JPEG compression", []string{"auto", "1", "2", "3", "4", "5"}, []byte{0, 13, 12, 11, 10, 9}, func(p *vens.ScanConfig) *byte { return &p.CompressionArg }),
		scanNumber("bw-density", "B&W density (-5 to +5)", "-5 to +5 (B&W only)", -5, 5, func(p *vens.ScanConfig) *int { return &p.BWDensity }),
		scanToggle("multi-feed", "Multi-feed detection", func(p *vens.ScanConfig) *bool { return &p.MultiFeed }),
		scanToggle("bleed-through", "Reduce bleed-through", func(p *vens.ScanConfig) *bool { return &p.BleedThrough }),
		scanToggle("blank-pages", "Remove blank pages", func(p *vens.ScanConfig) *bool { return &p.BlankPageRemoval }),
	}
}
