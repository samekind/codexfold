//go:build ignore

// Draw the Windows storage glyph at multiple resolutions without external assets.
// Run from cmd/codexfold-tray: go run assets/generate.go
package main

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
)

func rounded(x, y, left, top, right, bottom, radius float64) bool {
	dx := math.Max(math.Max(left+radius-x, 0), x-(right-radius))
	dy := math.Max(math.Max(top+radius-y, 0), y-(bottom-radius))
	return x >= left && x <= right && y >= top && y <= bottom && dx*dx+dy*dy <= radius*radius
}

func glyph(size int, warning bool) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	const samples = 4
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			var r, g, b, a float64
			for sy := 0; sy < samples; sy++ {
				for sx := 0; sx < samples; sx++ {
					px := (float64(x) + (float64(sx)+.5)/samples) * 64 / float64(size)
					py := (float64(y) + (float64(sy)+.5)/samples) * 64 / float64(size)
					c := color.NRGBA{}
					if rounded(px, py, 2, 2, 62, 62, 14) {
						c = color.NRGBA{uint8(66 - py*.13), uint8(125 - py*.2), uint8(201 - py*.15), 255}
					}
					// Stacked sheets flowing into a compact storage drive.
					if rounded(px, py, 19, 15, 45, 18, 1.5) || rounded(px, py, 16, 22, 48, 25, 1.5) || rounded(px, py, 12, 30, 52, 49, 4) {
						c = color.NRGBA{248, 251, 255, 255}
					}
					if rounded(px, py, 18, 38, 35, 41, 1.5) || math.Hypot(px-44, py-39.5) <= 1.8 {
						c = color.NRGBA{58, 112, 185, 255}
					}
					if warning && math.Hypot(px-51, py-51) <= 10 {
						c = color.NRGBA{255, 255, 255, 255}
						if math.Hypot(px-51, py-51) <= 8 {
							c = color.NRGBA{208, 142, 39, 255}
						}
					}
					r += float64(c.R) * float64(c.A) / 255
					g += float64(c.G) * float64(c.A) / 255
					b += float64(c.B) * float64(c.A) / 255
					a += float64(c.A)
				}
			}
			if a > 0 {
				img.SetNRGBA(x, y, color.NRGBA{uint8(r * 255 / a), uint8(g * 255 / a), uint8(b * 255 / a), uint8(a / (samples * samples))})
			}
		}
	}
	return img
}

func writeIcon(name string, warning bool) {
	sizes := []int{16, 20, 24, 32, 40, 48, 64, 128, 256}
	var frames [][]byte
	for _, size := range sizes {
		var frame bytes.Buffer
		if err := png.Encode(&frame, glyph(size, warning)); err != nil {
			panic(err)
		}
		frames = append(frames, frame.Bytes())
	}
	var output bytes.Buffer
	write := func(value any) {
		if err := binary.Write(&output, binary.LittleEndian, value); err != nil {
			panic(err)
		}
	}
	write(uint16(0))
	write(uint16(1))
	write(uint16(len(sizes)))
	offset := 6 + 16*len(sizes)
	for i, size := range sizes {
		write([4]byte{byte(size % 256), byte(size % 256), 0, 0})
		write(uint16(1))
		write(uint16(32))
		write(uint32(len(frames[i])))
		write(uint32(offset))
		offset += len(frames[i])
	}
	for _, frame := range frames {
		output.Write(frame)
	}
	if err := os.WriteFile("assets/"+name+".ico", output.Bytes(), 0644); err != nil {
		panic(err)
	}
}

func main() { writeIcon("codexfold", false); writeIcon("codexfold-attention", true) }
