package vens

import (
	"bytes"
	"encoding/binary"
	"golang.org/x/image/tiff"
	"image"
	"image/jpeg"
	"testing"
)

func TestJPEGEndSkipsThumbnailAndPartialMarkers(t *testing.T) {
	var original bytes.Buffer
	if err := jpeg.Encode(&original, image.NewGray(image.Rect(0, 0, 8, 8)), nil); err != nil {
		t.Fatal(err)
	}
	thumbnail := original.Bytes()
	app := append([]byte{0xff, 0xe1, 0, 0}, thumbnail...)
	binary.BigEndian.PutUint16(app[2:4], uint16(len(thumbnail)+2))
	data := append([]byte{0xff, 0xd8}, app...)
	data = append(data, original.Bytes()[2:]...)
	for i := 0; i < len(data); i++ {
		end, err := jpegEnd(data[:i])
		if err != nil || end != 0 {
			t.Fatalf("partial %d: end=%d err=%v", i, end, err)
		}
	}
	end, err := jpegEnd(append(data, 0, 0))
	if err != nil || end != len(data) {
		t.Fatalf("end=%d want=%d err=%v", end, len(data), err)
	}
}

func TestTIFFEndAndActualEncoding(t *testing.T) {
	var data bytes.Buffer
	if err := tiff.Encode(&data, image.NewGray(image.Rect(0, 0, 8, 8)), nil); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < len(data.Bytes()); i++ {
		end, err := tiffEnd(data.Bytes()[:i])
		if err != nil || end != 0 {
			t.Fatalf("partial TIFF %d: %d %v", i, end, err)
		}
	}
	end, err := tiffEnd(data.Bytes())
	if err != nil || end != data.Len() {
		t.Fatal(end, data.Len(), err)
	}
	p := Page{JPEG: data.Bytes()}
	_, mime, err := p.ImageInfo()
	if err != nil || mime != "image/tiff" {
		t.Fatal(mime, err)
	}
	if mode, err := p.ImageColorMode(); err != nil || mode != ColorGray {
		t.Fatal("8-bit TIFF mislabeled bitonal", mode, err)
	}
}
