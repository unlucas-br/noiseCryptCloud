# noiseCryptCloud (ncc)

Store files inside YouTube videos. Resilient to YouTube's compression using Reed-Solomon error correction.

## Features

- **Resilient Encoding**: 4×4 macro-pixels survive H.264/VP9/AV1 transcoding
- **Error Correction**: Reed-Solomon 48/16 (75% redundancy) (300% overhead)
- **Integrity**: SHA-256 global hash + CRC32 per frame
- **Encryption**: ChaCha20-Poly1305 with Argon2id key derivation
- **Progress UI**: Beautiful terminal interface with Bubble Tea

## Installation

```bash
# Install dependencies
go mod download

# Build
go build -o ncc ./cmd/cli

# Or use make
make build
```

## Requirements

- **Go 1.21+**
- **FFmpeg** (must be in PATH)
- **yt-dlp** (optional, for downloading from YouTube)

## Usage

### Encode a file to video

```bash
ncc -mode=encode -input="document.pdf" -output="backup.avi"

# With encryption
ncc -mode=encode -input="document.pdf" -output="backup.avi" -password="secret"
```

### Decode video back to file

```bash
ncc  -mode=decode -input="backup.avi" -output="document_recovered.pdf"

# With decryption
ncc -mode=decode -input="backup.avi" -output="document_recovered.pdf" -password="secret"
```

## How It Works

1. **Encoding**:
   - File is optionally encrypted with ChaCha20-Poly1305
   - Data is encoded in **Robust Mode** to survive YouTube compression
   - Reed-Solomon ECC adds **75% redundancy**
   - FFmpeg compiles frames into lossless AVI video

2. **Decoding**:
   - FFmpeg extracts frames from video
   - Decoder **auto-calibrates** based on frame content
   - Reed-Solomon corrects up to 75% data corruption
   - SHA-256 verifies file integrity

## Technical Details (Robust Mode)

| Parameter | Value |
|-----------|-------|
| Resolution | 1280×720 |
| Macro-pixel size | **16×16 pixels** |
| Encoding | **Binary (Black/White)** |
| Data shards | 16 |
| Parity shards | 48 |
| ECC overhead | **300% (75% of total is parity)** |
| Capacity/frame | ~35-100 bytes (varies) |
| Calibration | Automatic per-frame |

## Project Structure

```
ncc/
├── cmd/cli/main.go           # CLI with Bubble Tea UI
├── internal/
│   ├── encoder/
│   │   ├── macro_pixel.go    # Byte → RGB (YUV-safe)
│   │   ├── reed_solomon.go   # ECC wrapper
│   │   ├── framer.go         # Frame structure
│   │   └── video.go          # FFmpeg encoder
│   ├── decoder/
│   │   ├── extractor.go      # Frame extraction
│   │   └── reconstructor.go  # Data reconstruction
│   └── crypto/
│       └── encrypt.go        # ChaCha20 + Argon2
├── pkg/utils/checksum.go     # Hash helpers
├── go.mod
├── Makefile
└── README.md
```

## License

MIT

