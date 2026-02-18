package encoder

import (
	"bytes"
	"fmt"
	"io"

	"github.com/klauspost/reedsolomon"
)

type ECCConfig struct {
	DataShards   int
	ParityShards int
}

func NewECCConfig(level string) ECCConfig {
	switch level {
	case "low":
		// ~25% redundância (16 dados, 4 paridade)
		return ECCConfig{DataShards: 16, ParityShards: 4}
	case "high":
		// ~200% redundância (16 dados, 32 paridade)
		return ECCConfig{DataShards: 16, ParityShards: 32}
	default: // "medium"
		// ~50% redundância (16 dados, 8 paridade)
		return ECCConfig{DataShards: 16, ParityShards: 8}
	}
}

type ECCEncoder struct {
	enc    reedsolomon.Encoder
	Config ECCConfig // Exportado para acesso externo
}

func NewECCEncoder(cfg ECCConfig) (*ECCEncoder, error) {
	enc, err := reedsolomon.New(cfg.DataShards, cfg.ParityShards)
	if err != nil {
		return nil, fmt.Errorf("failed to create RS encoder: %w", err)
	}
	return &ECCEncoder{enc: enc, Config: cfg}, nil
}

func (e *ECCEncoder) Encode(data []byte) ([][]byte, error) {
	// Importante: Copiar dados pois Split modifica o slice (padding)
	dataCopy := make([]byte, len(data))
	copy(dataCopy, data)

	shards, err := e.enc.Split(dataCopy)
	if err != nil {
		return nil, fmt.Errorf("split failed: %w", err)
	}
	if err := e.enc.Encode(shards); err != nil {
		return nil, fmt.Errorf("encode failed: %w", err)
	}
	return shards, nil
}

func (e *ECCEncoder) Verify(shards [][]byte) (bool, error) {
	return e.enc.Verify(shards)
}

func (e *ECCEncoder) Reconstruct(shards [][]byte) error {
	return e.enc.Reconstruct(shards)
}

func (e *ECCEncoder) Join(shards [][]byte, outSize int) ([]byte, error) {
	buf := new(bytes.Buffer)
	err := e.enc.Join(io.Writer(buf), shards, outSize)
	if err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
