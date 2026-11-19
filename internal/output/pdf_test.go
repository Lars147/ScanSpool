package output

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"testing"

	"golang.org/x/image/tiff"

	"scanspool/internal/config"
	"scanspool/internal/vens"
)

func TestDuplexPDFPreservesJPEGAndQueuesCompleteDocument(t *testing.T) {
	var source bytes.Buffer
	if err := jpeg.Encode(&source, image.NewRGBA(image.Rect(0, 0, 170, 90)), nil); err != nil {
		t.Fatal(err)
	}
	page := vens.Page{JPEG: source.Bytes()}
	data, err := PDF([]vens.Page{page, page})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(data, []byte("%PDF-")) || !bytes.HasSuffix(bytes.TrimSpace(data), []byte("%%EOF")) {
		t.Fatal("invalid PDF envelope")
	}
	if bytes.Count(data, []byte("/Type /Page\n")) != 2 {
		t.Fatal("duplex pages missing")
	}
	if !bytes.Contains(data, source.Bytes()) || !bytes.Contains(data, []byte("/MediaBox [0 0 40.80 21.60]")) {
		t.Fatal("JPEG changed or 300 dpi landscape dimensions incorrect")
	}
	target, server := ftpServer(t, false)
	dir := t.TempDir()
	name, err := Enqueue(dir, config.Destination{FTP: target}, data)
	if err != nil {
		t.Fatal(err)
	}
	jobs, err := Pending(dir)
	if err != nil || len(jobs) != 1 {
		t.Fatal("missing completed job", err)
	}
	if err := Send(context.Background(), dir, config.Destination{FTP: target}, jobs[0]); err != nil {
		t.Fatal(err)
	}
	server.mu.Lock()
	defer server.mu.Unlock()
	if !bytes.Equal(server.files[name], data) {
		t.Fatal("delivered PDF differs")
	}
}

func TestInvalidPageNeverProducesPDF(t *testing.T) {
	for _, pages := range [][]vens.Page{nil, {{JPEG: []byte("broken JPEG")}}} {
		if _, err := PDF(pages); err == nil {
			t.Fatal("accepted missing or invalid pages")
		}
	}
}

func TestBlankPageRemovalAndTIFFPDF(t *testing.T) {
	var blank, marked bytes.Buffer
	page := image.NewGray(image.Rect(0, 0, 100, 50))
	draw.Draw(page, page.Bounds(), image.White, image.Point{}, draw.Src)
	if err := tiff.Encode(&blank, page, nil); err != nil {
		t.Fatal(err)
	}
	page.SetGray(50, 25, color.Gray{Y: 150})
	if err := tiff.Encode(&marked, page, nil); err != nil {
		t.Fatal(err)
	}
	pages := []vens.Page{{JPEG: blank.Bytes()}, {JPEG: marked.Bytes(), PixelSize: &vens.PixelSizeInfo{XRes: 200}}}
	kept, err := RemoveBlankPages(context.Background(), pages)
	if err != nil || len(kept) != 1 || !bytes.Equal(kept[0].JPEG, marked.Bytes()) {
		t.Fatal("blank filter failed to preserve a faint mark", err)
	}
	data, err := PDF(kept)
	if err != nil || !bytes.Contains(data, []byte("/MediaBox [0 0 36.00 18.00]")) || bytes.Count(data, []byte("/Type /Page\n")) != 1 {
		t.Fatal("TIFF not embedded at its actual 200 dpi", err)
	}
}
