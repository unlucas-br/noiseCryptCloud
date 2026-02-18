package cluster

import (
	"fmt"

	"github.com/klauspost/compress/zstd"
)

// Shared encoder/decoder (thread-safe, reusable)
var (
	zstdEncoder *zstd.Encoder
	zstdDecoder *zstd.Decoder
)

func init() {
	var err error
	// Speed level 3 (fastest) — we want throughput, not max compression
	zstdEncoder, err = zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedFastest))
	if err != nil {
		panic(fmt.Sprintf("failed to create zstd encoder: %v", err))
	}

	zstdDecoder, err = zstd.NewReader(nil)
	if err != nil {
		panic(fmt.Sprintf("failed to create zstd decoder: %v", err))
	}
}

// CompressPixels compresses RGBA pixel data using zstd
// Typical compression: 3.6MB → ~50-100KB (macro-pixel data is very repetitive)
func CompressPixels(rgba []byte) []byte {
	return zstdEncoder.EncodeAll(rgba, make([]byte, 0, len(rgba)/20))
}

// DecompressPixels decompresses zstd-compressed pixel data
func DecompressPixels(compressed []byte) ([]byte, error) {
	data, err := zstdDecoder.DecodeAll(compressed, nil)
	if err != nil {
		return nil, fmt.Errorf("zstd decompress: %w", err)
	}
	return data, nil
}
