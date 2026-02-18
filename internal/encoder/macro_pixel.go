package encoder

import (
	"image"
	"image/color"
)

// MacroPixel represents a block of pixels encoding data
// Uses 4-level grayscale (2 bits per pixel) or 2-level binary (1 bit per pixel)
// 4 levels: 0=black(32), 1=dark(96), 2=light(160), 3=white(224)
// 2 levels: 0=black(32), 1=white(224)
type MacroPixel struct {
	X, Y     int
	DataByte byte // Lower 2 bits used (0-3) for gray, 1 bit (0-1) for binary
	Size     int
	IsBinary bool // If true, uses high-contrast binary encoding
}

// 4 gray levels with maximum spacing (64 units apart, well within error margin)
var grayLevels = [4]uint8{32, 96, 160, 224}

// Binary levels for maximum robustness (contrast)
var binaryLevels = [2]uint8{32, 224}

// NibbleToGray converts a 2-bit value (0-3) to a grayscale value
func NibbleToGray(bits byte) uint8 {
	if bits > 3 {
		bits = 3
	}
	return grayLevels[bits]
}

// BitToGray converts a 1-bit value (0-1) to a grayscale value (binary)
func BitToGray(bit byte) uint8 {
	if bit > 1 {
		bit = 1
	}
	return binaryLevels[bit]
}

// GrayToNibble converts a grayscale value back to a 2-bit value with tolerance
// Threshold midpoints: 64, 128, 192
func GrayToNibble(gray uint8) byte {
	if gray < 64 {
		return 0
	} else if gray < 128 {
		return 1
	} else if gray < 192 {
		return 2
	}
	return 3
}

// DynGrayToNibble converts a gray value to 2-bit using custom thresholds
// thresholds[0] = limit 0/1 (between black/dark)
// thresholds[1] = limit 1/2 (between dark/light)
// thresholds[2] = limit 2/3 (between light/white)
func DynGrayToNibble(gray uint8, thresholds [3]uint8) byte {
	if gray < thresholds[0] {
		return 0
	} else if gray < thresholds[1] {
		return 1
	} else if gray < thresholds[2] {
		return 2
	}
	return 3
}

// ByteToGray converts data to gray level based on mode
func (mp *MacroPixel) ByteToGray() uint8 {
	if mp.IsBinary {
		return BitToGray(mp.DataByte & 0x01)
	}
	return NibbleToGray(mp.DataByte & 0x03)
}

// GrayToByte returns a 2-bit value (0-3)
func GrayToByte(gray uint8) byte {
	return GrayToNibble(gray)
}

// Render creates an image for this macro pixel
func (mp *MacroPixel) Render() *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, mp.Size, mp.Size))
	gray := mp.ByteToGray()
	c := color.RGBA{R: gray, G: gray, B: gray, A: 255}

	for y := 0; y < mp.Size; y++ {
		for x := 0; x < mp.Size; x++ {
			img.Set(x, y, c)
		}
	}
	return img
}

// ExpandByte takes a byte and returns 4 pairs of 2 bits each
func ExpandByte(b byte) [4]byte {
	return [4]byte{
		(b >> 6) & 0x03, // bits 7-6
		(b >> 4) & 0x03, // bits 5-4
		(b >> 2) & 0x03, // bits 3-2
		b & 0x03,        // bits 1-0
	}
}

// CombineBits combines 4 pairs of 2-bit values into a byte
func CombineBits(bits [4]byte) byte {
	return (bits[0] << 6) | (bits[1] << 4) | (bits[2] << 2) | bits[3]
}

// CombineNibbles kept for compatibility - now combines 2 2-bit values into 4 bits
func CombineNibbles(high, low byte) byte {
	return ((high & 0x03) << 2) | (low & 0x03)
}

// ColorSpace kept for compatibility
type ColorSpace struct {
	Y, U, V uint8
}

func (mp *MacroPixel) ByteToColor() ColorSpace {
	gray := mp.ByteToGray()
	return ColorSpace{Y: gray, U: 128, V: 128}
}

func YUVToRGB(y, u, v uint8) color.RGBA {
	return color.RGBA{R: y, G: y, B: y, A: 255}
}

func clampUint8(v float64) uint8 {
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return uint8(v)
}
