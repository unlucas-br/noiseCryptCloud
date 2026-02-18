package cluster

import (
	"bytes"
	"encoding/gob"
	"encoding/json"
	"fmt"
)

// JobConfig: Parâmetros de encode enviados ao conectar
type JobConfig struct {
	// Configuração de Frame
	Width             int `json:"width"`
	Height            int `json:"height"`
	MacroSize         int `json:"macroSize"`
	FPS               int `json:"fps"`
	CalibrationHeight int `json:"calibrationHeight"`
	GrayLevels        int `json:"grayLevels"`

	// Configuração ECC
	DataShards   int `json:"dataShards"`
	ParityShards int `json:"parityShards"`

	// Metadados do Arquivo
	TotalFrames  int      `json:"totalFrames"`
	OriginalSize uint64   `json:"originalSize"`
	FileHash     [32]byte `json:"fileHash"`
}

// FrameJob: Frame individual para processamento
type FrameJob struct {
	FrameIndex int
	Data       []byte // Chunk bruto de dados
}

// FrameResult: Resultado do processamento
type FrameResult struct {
	FrameIndex       int
	CompressedPixels []byte // Pixels RGBA (zstd)
	Width            int
	Height           int
	Error            string // Vazio se OK
}

// WorkerInfo: Capacidades do worker
type WorkerInfo struct {
	Hostname string `json:"hostname"`
	CPUCores int    `json:"cpuCores"`
	OS       string `json:"os"`
	Arch     string `json:"arch"`
}

// ---- Encoding GOB (Binário eficiente) ----

func EncodeGob(v interface{}) ([]byte, error) {
	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(v); err != nil {
		return nil, fmt.Errorf("gob encode: %w", err)
	}
	return buf.Bytes(), nil
}

func DecodeGob(data []byte, v interface{}) error {
	return gob.NewDecoder(bytes.NewReader(data)).Decode(v)
}

// ---- Encoding JSON (Legível) ----

func EncodeJSON(v interface{}) ([]byte, error) {
	return json.Marshal(v)
}

func DecodeJSON(data []byte, v interface{}) error {
	return json.Unmarshal(data, v)
}
