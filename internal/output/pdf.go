package output

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/png"

	"codeberg.org/go-pdf/fpdf"
	"scanspool/internal/vens"
)

// PDF preserves JPEG pages and embeds TIFF pages losslessly as PNG.
func PDF(pages []vens.Page) ([]byte, error) {
	if len(pages) == 0 {
		return nil, fmt.Errorf("no pages")
	}
	pdf := fpdf.New("P", "mm", "", "")
	pdf.SetAutoPageBreak(false, 0)
	for i, page := range pages {
		info, format, err := page.ImageInfo()
		if err != nil {
			return nil, err
		}
		dpi := vens.ImageDPI(page.JPEG)
		if page.PixelSize != nil && page.PixelSize.XRes > 0 {
			dpi = page.PixelSize.XRes
		}
		if dpi == 0 {
			dpi = 300
		} // The bundled profile has a fixed, known resolution.
		width, height := float64(info.Width)*25.4/float64(dpi), float64(info.Height)*25.4/float64(dpi)
		pdf.AddPageFormat("P", fpdf.SizeType{Wd: width, Ht: height})
		name := fmt.Sprintf("page%d", i)
		data := page.JPEG
		options := fpdf.ImageOptions{ImageType: "JPEG"}
		if format == "image/tiff" {
			decoded, _, err := image.Decode(bytes.NewReader(data))
			if err != nil {
				return nil, fmt.Errorf("decode TIFF page %d: %w", i+1, err)
			}
			var converted bytes.Buffer
			if err := png.Encode(&converted, decoded); err != nil {
				return nil, fmt.Errorf("encode TIFF page %d for PDF: %w", i+1, err)
			}
			data = converted.Bytes()
			options.ImageType = "PNG"
		}
		pdf.RegisterImageOptionsReader(name, options, bytes.NewReader(data))
		pdf.ImageOptions(name, 0, 0, width, height, false, options, 0, "")
	}
	var data bytes.Buffer
	if err := pdf.Output(&data); err != nil {
		return nil, err
	}
	return data.Bytes(), nil
}

// RemoveBlankPages only drops uniformly near-white pages. Shadows, marks and
// colored paper are retained rather than risking loss of faint content.
func RemoveBlankPages(ctx context.Context, pages []vens.Page) ([]vens.Page, error) {
	kept := make([]vens.Page, 0, len(pages))
	for _, page := range pages {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if _, _, err := page.ImageInfo(); err != nil {
			return nil, err
		}
		decoded, _, err := image.Decode(bytes.NewReader(page.JPEG))
		if err != nil {
			return nil, err
		}
		if !nearWhite(decoded) {
			kept = append(kept, page)
		}
	}
	return kept, nil
}

func nearWhite(img image.Image) bool {
	const minimum = 250 * 257
	bounds := img.Bounds()
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			r, g, b, a := img.At(x, y).RGBA()
			if a != 0xffff || r < minimum || g < minimum || b < minimum {
				return false
			}
		}
	}
	return true
}
