package main

import (
	"fmt"
	"image"
	"image/png"
	"os"

	"ncc/internal/encoder"
)

func main() {
	// Test the gray levels
	fmt.Println("Testing 4-level grayscale encoding:")
	for b := byte(0); b <= 3; b++ {
		gray := encoder.NibbleToGray(b)
		decoded := encoder.GrayToByte(gray)
		status := "✓"
		if b != decoded {
			status = "✗"
		}
		fmt.Printf("  %d -> gray %d -> decoded %d %s\n", b, gray, decoded, status)
	}

	// Test edge cases around thresholds
	fmt.Println("\nTesting threshold edge cases:")
	testGrays := []uint8{31, 32, 33, 63, 64, 65, 95, 96, 97, 127, 128, 129, 159, 160, 161, 191, 192, 193, 223, 224, 225}
	for _, g := range testGrays {
		decoded := encoder.GrayToByte(g)
		expected := byte(0)
		if g >= 64 && g < 128 {
			expected = 1
		} else if g >= 128 && g < 192 {
			expected = 2
		} else if g >= 192 {
			expected = 3
		}
		status := "✓"
		if decoded != expected {
			status = "✗"
		}
		fmt.Printf("  gray %d -> %d (expected %d) %s\n", g, decoded, expected, status)
	}

	// Test NCC1 magic bytes
	testBytes := []byte{'N', 'C', 'C', '1'} // 78, 67, 67, 49
	fmt.Printf("\nTesting magic bytes NCC1:\n")
	for _, b := range testBytes {
		bits := [4]byte{
			(b >> 6) & 0x03,
			(b >> 4) & 0x03,
			(b >> 2) & 0x03,
			b & 0x03,
		}
		fmt.Printf("  '%c' (0x%02X) -> bits [%d,%d,%d,%d] -> grays [%d,%d,%d,%d]\n",
			b, b, bits[0], bits[1], bits[2], bits[3],
			encoder.NibbleToGray(bits[0]),
			encoder.NibbleToGray(bits[1]),
			encoder.NibbleToGray(bits[2]),
			encoder.NibbleToGray(bits[3]),
		)
	}

	// Create a simple test frame with known values
	fmt.Println("\nCreating test frame...")
	macroSize := 4
	width := len(testBytes) * 4 * macroSize // 4 macropixels per byte
	height := macroSize

	img := image.NewRGBA(image.Rect(0, 0, width, height))

	// Set pixels manually with correct gray values
	x := 0
	for _, b := range testBytes {
		bits := [4]byte{
			(b >> 6) & 0x03,
			(b >> 4) & 0x03,
			(b >> 2) & 0x03,
			b & 0x03,
		}
		for _, bit2 := range bits {
			gray := encoder.NibbleToGray(bit2)
			for dy := 0; dy < macroSize; dy++ {
				for dx := 0; dx < macroSize; dx++ {
					img.Pix[(dy*img.Stride)+(x*macroSize+dx)*4+0] = gray // R
					img.Pix[(dy*img.Stride)+(x*macroSize+dx)*4+1] = gray // G
					img.Pix[(dy*img.Stride)+(x*macroSize+dx)*4+2] = gray // B
					img.Pix[(dy*img.Stride)+(x*macroSize+dx)*4+3] = 255  // A
				}
			}
			x++
		}
	}

	// Save test frame
	f, err := os.Create("test_frame.png")
	if err != nil {
		panic(err)
	}
	png.Encode(f, img)
	f.Close()
	fmt.Printf("Created test_frame.png (%dx%d)\n", width, height)

	// Now read it back
	f2, _ := os.Open("test_frame.png")
	img2, _, _ := image.Decode(f2)
	f2.Close()

	// Extract values
	fmt.Println("\nExtracting values from test frame:")
	var nibbles []byte
	for i := 0; i < 16; i++ { // 16 macropixels = 4 bytes
		startX := i * macroSize
		var sumR uint32
		for dy := 0; dy < macroSize; dy++ {
			for dx := 0; dx < macroSize; dx++ {
				r, _, _, _ := img2.At(startX+dx, dy).RGBA()
				sumR += r >> 8
			}
		}
		avgR := uint8(sumR / 16)
		nibble := encoder.GrayToByte(avgR)
		nibbles = append(nibbles, nibble)
		fmt.Printf("  Macro %d: avgR=%d -> nibble=%d\n", i, avgR, nibble)
	}

	// Combine nibbles into bytes
	fmt.Println("\nRecovered bytes:")
	allMatch := true
	for i := 0; i+3 < len(nibbles); i += 4 {
		b := (nibbles[i] << 6) | (nibbles[i+1] << 4) | (nibbles[i+2] << 2) | nibbles[i+3]
		match := b == testBytes[i/4]
		if !match {
			allMatch = false
		}
		status := "✓"
		if !match {
			status = "✗"
		}
		fmt.Printf("  Byte %d: 0x%02X '%c' (original: 0x%02X '%c') %s\n", i/4, b, b, testBytes[i/4], testBytes[i/4], status)
	}

	if allMatch {
		fmt.Println("\n✅ ALL BYTES MATCH!")
	} else {
		fmt.Println("\n❌ SOME BYTES DO NOT MATCH")
	}
}
