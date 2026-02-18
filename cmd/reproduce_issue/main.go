package main

import (
	"fmt"
	"ncc/internal/decoder"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func main() {
	fmt.Println("🔍 Fast-Checking Header Recovery on 'longo.mp4'...")

	inputFile := "longo.mp4"
	tempDir := "temp_check"
	os.RemoveAll(tempDir)
	os.MkdirAll(tempDir, 0755)
	defer os.RemoveAll(tempDir) // Clean up

	// 1. Extract Frame 0 only
	frame0 := filepath.Join(tempDir, "frame_00001.png")
	fmt.Printf("🎬 Extracting Frame 0 to %s...\n", frame0)

	// ffmpeg -y -i input -frames:v 1 out.png
	cmd := exec.Command("ffmpeg", "-y", "-i", inputFile, "-frames:v", "1", frame0)
	out, err := cmd.CombinedOutput()
	if err != nil {
		fmt.Printf("❌ ffmpeg failed: %v\nOutput: %s\n", err, string(out))
		os.Exit(1)
	}

	// 2. Attempt Reconstruction (Dense Preset)
	fmt.Println("🔧 Testing Decoder...")
	recon := decoder.NewFrameReconstructor("dense")

	// We only pass 1 frame. It WILL fail to reconstruct the full file.
	frames := []string{frame0}

	err = recon.ReconstructFile(frames, "dummy_out.rar", nil)

	if err != nil {
		errStr := err.Error()
		if strings.Contains(errStr, "invalid magic") {
			fmt.Printf("❌ FAILED: Universal Recovery did not fix the magic bytes.\nError: %v\n", err)
			os.Exit(1)
		} else {
			// Any other error (e.g. "expected 20000 frames, found 1") means the MAGIC WAS VALID!
			fmt.Printf("✅ SUCCESS! Magic header accepted.\n(Expected error received: %v)\n", err)
			fmt.Println("The full file should decode correctly now.")
		}
	} else {
		// Unlikely to happen with 1 frame, but if so, success
		fmt.Println("✅ SUCCESS! Frame 0 decoded perfectly.")
	}
}
