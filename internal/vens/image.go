package vens

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	_ "image/jpeg"

	_ "golang.org/x/image/tiff"
)

// jpegEnd walks marker segments and entropy-coded data. APP/EXIF data can contain
// complete thumbnail JPEGs; searching for their EOI would truncate the main image.
func jpegEnd(data []byte) (int, error) {
	if len(data) < 2 {
		return 0, nil
	}
	if data[0] != 0xff || data[1] != 0xd8 {
		return 0, fmt.Errorf("invalid JPEG start")
	}
	entropy := false
	for pos := 2; pos < len(data); {
		if entropy {
			for pos < len(data) && data[pos] != 0xff {
				pos++
			}
			if pos+1 >= len(data) {
				return 0, nil
			}
			if data[pos+1] == 0 || (data[pos+1] >= 0xd0 && data[pos+1] <= 0xd7) {
				pos += 2
				continue
			}
		}
		if data[pos] != 0xff {
			return 0, fmt.Errorf("invalid JPEG marker")
		}
		for pos < len(data) && data[pos] == 0xff {
			pos++
		}
		if pos >= len(data) {
			return 0, nil
		}
		marker := data[pos]
		pos++
		if marker == 0xd9 {
			return pos, nil
		}
		if marker == 0x01 || (marker >= 0xd0 && marker <= 0xd7) {
			continue
		}
		if marker == 0xd8 || marker == 0 {
			return 0, fmt.Errorf("unexpected JPEG marker %02x", marker)
		}
		if pos+2 > len(data) {
			return 0, nil
		}
		length := int(binary.BigEndian.Uint16(data[pos : pos+2]))
		if length < 2 {
			return 0, fmt.Errorf("invalid JPEG segment length")
		}
		if length > len(data)-pos {
			return 0, nil
		}
		pos += length
		entropy = marker == 0xda || (entropy && marker == 0xdc)
	}
	return 0, nil
}

// ImageInfo reports the actual encoding and colour model, independent of the
// requested mode (automatic colour detection can vary between pages).
func (p Page) ImageInfo() (image.Config, string, error) {
	info, format, err := image.DecodeConfig(bytes.NewReader(p.JPEG))
	if err != nil {
		return info, "", err
	}
	if info.Width <= 0 || info.Height <= 0 || uint64(info.Width)*uint64(info.Height) > 100_000_000 {
		return info, "", fmt.Errorf("page dimensions exceed supported scan size")
	}
	switch format {
	case "jpeg":
		return info, "image/jpeg", nil
	case "tiff":
		return info, "image/tiff", nil
	default:
		return info, "", fmt.Errorf("unsupported page encoding %q", format)
	}
}
func (p Page) ImageColorMode() (ColorMode, error) {
	info, format, err := p.ImageInfo()
	if err != nil {
		return ColorAuto, err
	}
	if format == "image/tiff" && tiffBitonal(p.JPEG) {
		return ColorBW, nil
	}
	if info.ColorModel == color.GrayModel || info.ColorModel == color.Gray16Model {
		return ColorGray, nil
	}
	return ColorColor, nil
}

// tiffEnd bounds a classic TIFF through its directory, value tables and image
// strips/tiles. It returns zero while a referenced range is still incomplete.
func tiffEnd(data []byte) (int, error) {
	if len(data) < 8 {
		return 0, nil
	}
	order, err := tiffOrder(data)
	if err != nil {
		return 0, err
	}
	offset := uint64(order.Uint32(data[4:8]))
	end := uint64(8)
	for directories := 0; offset != 0; directories++ {
		if directories >= 16 {
			return 0, fmt.Errorf("too many TIFF directories")
		}
		if offset+2 > uint64(len(data)) {
			return 0, nil
		}
		count := uint64(order.Uint16(data[offset : offset+2]))
		if count > 4096 {
			return 0, fmt.Errorf("TIFF directory too large")
		}
		next := offset + 2 + count*12
		if next+4 > uint64(len(data)) {
			return 0, nil
		}
		if next+4 > end {
			end = next + 4
		}
		values := map[uint16][]uint64{}
		for i := uint64(0); i < count; i++ {
			entry := data[offset+2+i*12 : offset+14+i*12]
			tag, kind := order.Uint16(entry[:2]), order.Uint16(entry[2:4])
			n := uint64(order.Uint32(entry[4:8]))
			width := uint64(0)
			switch kind {
			case 1, 2, 6, 7:
				width = 1
			case 3, 8:
				width = 2
			case 4, 9, 11:
				width = 4
			case 5, 10, 12:
				width = 8
			default:
				return 0, fmt.Errorf("unsupported TIFF field type %d", kind)
			}
			size := n * width
			if size > 16*1024*1024 {
				return 0, fmt.Errorf("TIFF field too large")
			}
			value := entry[8:12]
			if size > 4 {
				start := uint64(order.Uint32(value))
				if start+size > uint64(len(data)) {
					return 0, nil
				}
				value = data[start : start+size]
				if start+size > end {
					end = start + size
				}
			}
			if tag == 273 || tag == 279 || tag == 324 || tag == 325 {
				if kind != 3 && kind != 4 {
					return 0, fmt.Errorf("invalid TIFF strip table")
				}
				if n > 65536 {
					return 0, fmt.Errorf("TIFF strip table too large")
				}
				items := make([]uint64, n)
				for j := uint64(0); j < n; j++ {
					if kind == 3 {
						items[j] = uint64(order.Uint16(value[j*2:]))
					} else {
						items[j] = uint64(order.Uint32(value[j*4:]))
					}
				}
				values[tag] = items
			}
		}
		found := false
		for _, pair := range [][2]uint16{{273, 279}, {324, 325}} {
			offsets, sizes := values[pair[0]], values[pair[1]]
			if len(offsets) == 0 && len(sizes) == 0 {
				continue
			}
			if len(offsets) != len(sizes) {
				return 0, fmt.Errorf("mismatched TIFF strip tables")
			}
			found = true
			for i, start := range offsets {
				limit := start + sizes[i]
				if limit > 16*1024*1024 {
					return 0, fmt.Errorf("TIFF image too large")
				}
				if limit > uint64(len(data)) {
					return 0, nil
				}
				if limit > end {
					end = limit
				}
			}
		}
		if !found {
			return 0, fmt.Errorf("TIFF has no image strips")
		}
		offset = uint64(order.Uint32(data[next : next+4]))
	}
	return int(end), nil
}
func tiffOrder(data []byte) (binary.ByteOrder, error) {
	if len(data) < 8 {
		return nil, fmt.Errorf("short TIFF header")
	}
	if bytes.Equal(data[:4], []byte{'I', 'I', 42, 0}) {
		return binary.LittleEndian, nil
	}
	if bytes.Equal(data[:4], []byte{'M', 'M', 0, 42}) {
		return binary.BigEndian, nil
	}
	return nil, fmt.Errorf("invalid TIFF header")
}
func tiffBitonal(data []byte) bool {
	order, err := tiffOrder(data)
	if err != nil {
		return false
	}
	offset := uint64(order.Uint32(data[4:8]))
	if offset+2 > uint64(len(data)) {
		return false
	}
	n := uint64(order.Uint16(data[offset:]))
	if n > 4096 || offset+2+n*12 > uint64(len(data)) {
		return false
	}
	// TIFF defaults to one bit per sample when BitsPerSample is absent.
	for i := uint64(0); i < n; i++ {
		e := data[offset+2+i*12 : offset+14+i*12]
		if order.Uint16(e) == 258 {
			return order.Uint16(e[2:4]) == 3 && order.Uint32(e[4:8]) == 1 && order.Uint16(e[8:10]) == 1
		}
	}
	return true
}

func ImageDPI(data []byte) int {
	if len(data) < 8 {
		return 0
	}
	// TIFF: starts with "II" (little-endian) or "MM" (big-endian)
	if (data[0] == 'I' && data[1] == 'I') || (data[0] == 'M' && data[1] == 'M') {
		return detectTIFFDPI(data)
	}
	// JPEG: starts with FF D8
	if data[0] == 0xFF && data[1] == 0xD8 {
		return detectJPEGDPI(data)
	}
	return 0
}

func detectTIFFDPI(data []byte) int {
	bo, err := tiffOrder(data)
	if err != nil {
		return 0
	}
	offset := uint64(bo.Uint32(data[4:8]))
	if offset+2 > uint64(len(data)) {
		return 0
	}
	n := uint64(bo.Uint16(data[offset : offset+2]))
	if n > 4096 || offset+2+n*12 > uint64(len(data)) {
		return 0
	}
	dpi := 0.0
	units := uint16(2)
	for i := uint64(0); i < n; i++ {
		entry := data[offset+2+i*12 : offset+14+i*12]
		tag := bo.Uint16(entry)
		if tag == 296 && bo.Uint16(entry[2:4]) == 3 && bo.Uint32(entry[4:8]) == 1 {
			units = bo.Uint16(entry[8:10])
		}
		if tag == 282 && bo.Uint16(entry[2:4]) == 5 && bo.Uint32(entry[4:8]) == 1 {
			at := uint64(bo.Uint32(entry[8:12]))
			if at+8 > uint64(len(data)) {
				return 0
			}
			num, den := bo.Uint32(data[at:at+4]), bo.Uint32(data[at+4:at+8])
			if den == 0 {
				return 0
			}
			dpi = float64(num) / float64(den)
		}
	}
	if units == 3 {
		dpi *= 2.54
	} else if units != 2 {
		return 0
	}
	if dpi < 1 || dpi > 9600 {
		return 0
	}
	return int(dpi + 0.5)
}
func detectJPEGDPI(data []byte) int {
	for i := 2; i+4 <= len(data); {
		if data[i] != 0xff {
			return 0
		}
		marker := data[i+1]
		if marker == 0xda || marker == 0xd9 {
			return 0
		}
		length := int(binary.BigEndian.Uint16(data[i+2 : i+4]))
		if length < 2 || i+2+length > len(data) {
			return 0
		}
		segment := data[i+4 : i+2+length]
		if marker == 0xe0 && len(segment) >= 14 && bytes.Equal(segment[:5], []byte("JFIF\x00")) {
			density := float64(binary.BigEndian.Uint16(segment[8:10]))
			if segment[7] == 2 {
				density *= 2.54
			} else if segment[7] != 1 {
				density = 0
			}
			if density >= 1 && density <= 9600 {
				return int(density + 0.5)
			}
		}
		if marker == 0xe1 && len(segment) > 6 && bytes.Equal(segment[:6], []byte("Exif\x00\x00")) {
			if dpi := detectTIFFDPI(segment[6:]); dpi > 0 {
				return dpi
			}
		}
		i += 2 + length
	}
	return 0
}
