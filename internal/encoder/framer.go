package encoder

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"hash/crc32"
)

// Constantes de estrutura e capacidade
const (
	FrameHeaderSizeBytes  = 18 // Tamanho do FrameHeader
	GlobalHeaderSizeBytes = 20 // Tamanho do GlobalHeader
	FrameFooterReserved   = 4  // Espaço reservado (footer)
	CalibrationBarHeight  = 16 // Altura da barra de calibração

	// Espaço reservado total (Header + Margem)
	ReservedMacrosPerFrame = 8 + FrameHeaderSizeBytes
)

// GlobalHeader: Metadados do arquivo (hash criptografado separadamente)
type GlobalHeader struct {
	OriginalSize uint64
	TotalFrames  uint32
	Reserved     [8]byte
}

func (gh GlobalHeader) Encode() []byte {
	buf := new(bytes.Buffer)
	binary.Write(buf, binary.BigEndian, gh.OriginalSize)
	binary.Write(buf, binary.BigEndian, gh.TotalFrames)
	buf.Write(gh.Reserved[:])
	return buf.Bytes()
}

func DecodeGlobalHeader(data []byte) (GlobalHeader, error) {
	var gh GlobalHeader
	if len(data) < GlobalHeaderSizeBytes {
		return gh, fmt.Errorf("insufficient data for GlobalHeader: got %d, need %d", len(data), GlobalHeaderSizeBytes)
	}
	buf := bytes.NewReader(data)
	binary.Read(buf, binary.BigEndian, &gh.OriginalSize)
	binary.Read(buf, binary.BigEndian, &gh.TotalFrames)
	buf.Read(gh.Reserved[:])
	return gh, nil
}

type FrameConfig struct {
	Width             int
	Height            int
	MacroSize         int
	FPS               int
	CalibrationHeight int // Altura reservada no topo para calibração
	GrayLevels        int // Níveis de cinza (2=P/B, 4=4-níveis)
}

func HighDensityFrameConfig() FrameConfig {
	return FrameConfig{
		Width:             1280, // 720p
		Height:            720,
		MacroSize:         10, // Reduzido 12->10
		FPS:               30, // FPS aumentado
		CalibrationHeight: 16,
		GrayLevels:        4, // Preto e branco de 4 níveis
	}
}

func YouTubeFrameConfig() FrameConfig {
	return FrameConfig{
		Width:             1920, // 1080p para melhor bitrate
		Height:            1080,
		MacroSize:         24, // Pixels maiores (resistência a compressão)
		FPS:               15, // FPS menor (menos compressão temporal)
		CalibrationHeight: 16,
		GrayLevels:        2, // Apenas binário
	}
}

func DefaultFrameConfig() FrameConfig {
	return FrameConfig{
		Width:             1280,
		Height:            720,
		MacroSize:         16, // Macropixels grandes
		FPS:               30,
		CalibrationHeight: 16, // 16px para calibração
		GrayLevels:        2,  // Modo binário (robustez)
	}
}

func (fc FrameConfig) GridSize() (cols, rows int) {
	cols = fc.Width / fc.MacroSize
	// Subtrair altura de calibração
	availableHeight := fc.Height - fc.CalibrationHeight
	rows = availableHeight / fc.MacroSize
	return
}

// CapacityPerFrame: Calcula bytes de DADOS por frame
// 2 bits (4 níveis): 4 pixels/byte
// 1 bit (2 níveis): 8 pixels/byte
func (fc FrameConfig) CapacityPerFrame(eccCfg ECCConfig, isFirstFrame bool) int {
	cols, rows := fc.GridSize()
	totalMacros := cols * rows

	var bytesInFrame int
	if fc.GrayLevels == 2 {
		bytesInFrame = totalMacros / 8 // 1 bit/pixel -> 8 px/byte
	} else {
		bytesInFrame = totalMacros / 4 // 2 bits/pixel -> 4 px/byte
	}

	// Reservar espaço para header (antes do ECC)
	availableForECC := bytesInFrame - FrameHeaderSizeBytes

	// ECC expande dados - fórmula segura contra arredondamento
	// reedsolomon.Split() usa ceil(len/DataShards) por shard
	// Calcular max shard size que cabe, depois derivar capacidade
	totalShards := eccCfg.DataShards + eccCfg.ParityShards
	maxShardSize := availableForECC / totalShards
	dataCapacity := maxShardSize * eccCfg.DataShards

	// Primeiro frame inclui GlobalHeader
	if isFirstFrame {
		dataCapacity -= GlobalHeaderSizeBytes
	}

	// Margem de segurança
	dataCapacity -= 10

	if dataCapacity < 0 {
		return 0
	}
	return dataCapacity
}

type FrameHeader struct {
	Magic        [4]byte
	FrameIndex   uint32
	DataSize     uint16
	DataCRC      uint32
	HasGlobal    uint8
	ParityShards uint8 // 0 = Legado (48), caso contrário shards de paridade
	GlobalOffset uint16
	GlobalMeta   GlobalHeader `binary:"-"`
}

// Encode: Serialização manual para robustez
func (fh FrameHeader) Encode() ([]byte, error) {
	buf := new(bytes.Buffer)

	// Escreve cada campo explicitamente (não depende de layout da struct)
	buf.Write(fh.Magic[:])
	binary.Write(buf, binary.BigEndian, fh.FrameIndex)
	binary.Write(buf, binary.BigEndian, fh.DataSize)
	binary.Write(buf, binary.BigEndian, fh.DataCRC)
	binary.Write(buf, binary.BigEndian, fh.HasGlobal)
	binary.Write(buf, binary.BigEndian, fh.ParityShards)
	binary.Write(buf, binary.BigEndian, fh.GlobalOffset)

	// Verifica tamanho esperado
	if buf.Len() != FrameHeaderSizeBytes {
		return nil, fmt.Errorf("FrameHeader size mismatch: got %d, expected %d", buf.Len(), FrameHeaderSizeBytes)
	}

	return buf.Bytes(), nil
}

// DecodeHeader: Leitura manual campo a campo
func DecodeHeader(data []byte) (FrameHeader, error) {
	var fh FrameHeader

	if len(data) < FrameHeaderSizeBytes {
		return fh, fmt.Errorf("insufficient data for FrameHeader: got %d, need %d", len(data), FrameHeaderSizeBytes)
	}

	buf := bytes.NewReader(data)

	// Lê cada campo explicitamente
	if _, err := buf.Read(fh.Magic[:]); err != nil {
		return fh, fmt.Errorf("read Magic: %w", err)
	}
	if err := binary.Read(buf, binary.BigEndian, &fh.FrameIndex); err != nil {
		return fh, fmt.Errorf("read FrameIndex: %w", err)
	}
	if err := binary.Read(buf, binary.BigEndian, &fh.DataSize); err != nil {
		return fh, fmt.Errorf("read DataSize: %w", err)
	}
	if err := binary.Read(buf, binary.BigEndian, &fh.DataCRC); err != nil {
		return fh, fmt.Errorf("read DataCRC: %w", err)
	}
	if err := binary.Read(buf, binary.BigEndian, &fh.HasGlobal); err != nil {
		return fh, fmt.Errorf("read HasGlobal: %w", err)
	}
	if err := binary.Read(buf, binary.BigEndian, &fh.ParityShards); err != nil {
		return fh, fmt.Errorf("read ParityShards: %w", err)
	}
	if err := binary.Read(buf, binary.BigEndian, &fh.GlobalOffset); err != nil {
		return fh, fmt.Errorf("read GlobalOffset: %w", err)
	}

	return fh, nil
}

type Frame struct {
	Config       FrameConfig
	Header       FrameHeader
	Data         []byte
	ECC          *ECCEncoder
	isFirstFrame bool
}

func NewFrame(cfg FrameConfig, ecc *ECCEncoder, index int, data []byte, totalFrames int, originalSize uint64, fileHash [32]byte) (*Frame, error) {
	fh := FrameHeader{
		Magic:        [4]byte{'N', 'C', 'C', '1'}, // Versão 1
		FrameIndex:   uint32(index),
		DataCRC:      0,
		HasGlobal:    0,
		ParityShards: uint8(ecc.Config.ParityShards),
	}

	var frameData []byte
	if index == 0 {
		fh.HasGlobal = 1
		fh.GlobalOffset = uint16(FrameHeaderSizeBytes) // GlobalHeader começa após FrameHeader

		// Segurança: Hash movido para payload criptografado
		gh := GlobalHeader{
			OriginalSize: 0, // Metadado ofuscado
			TotalFrames:  uint32(totalFrames),
		}
		frameData = append(gh.Encode(), data...)
		fh.DataSize = uint16(len(frameData))
		fh.DataCRC = crc32.ChecksumIEEE(frameData)
	} else {
		frameData = data
		fh.DataSize = uint16(len(data))
		fh.DataCRC = crc32.ChecksumIEEE(data)
	}

	return &Frame{
		Config:       cfg,
		Header:       fh,
		Data:         frameData,
		ECC:          ecc,
		isFirstFrame: index == 0,
	}, nil
}

func (f *Frame) Render(pixels []MacroPixel) ([]MacroPixel, error) {
	cols, rows := f.Config.GridSize()

	shards, err := f.ECC.Encode(f.Data)
	if err != nil {
		return nil, fmt.Errorf("ECC encode failed: %w", err)
	}

	var allBytes []byte
	for _, shard := range shards {
		allBytes = append(allBytes, shard...)
	}

	headerBytes, err := f.Header.Encode()
	if err != nil {
		return nil, err
	}
	allBytes = append(headerBytes, allBytes...)

	totalMacros := cols * rows

	var maxBytes int
	if f.Config.GrayLevels == 2 {
		maxBytes = totalMacros / 8
	} else {
		maxBytes = totalMacros / 4
	}

	// Segurança: Preencher padding com ruído aleatório
	if len(allBytes) < maxBytes {
		padding := make([]byte, maxBytes-len(allBytes))
		rand.Read(padding)
		allBytes = append(allBytes, padding...)
	}

	if len(allBytes) > maxBytes {
		return nil, fmt.Errorf("data too large for frame: %d bytes > %d max", len(allBytes), maxBytes)
	}

	// Expandir bytes em pixels
	// pixels := make([]MacroPixel, totalMacros)
	if cap(pixels) < totalMacros {
		pixels = make([]MacroPixel, totalMacros) // Buffer fallback se necessário
	}
	pixels = pixels[:totalMacros] // Ajustar tamanho

	pixelIdx := 0

	pixelsPerByte := 4
	if f.Config.GrayLevels == 2 {
		pixelsPerByte = 8
	}

	for y := 0; y < rows && pixelIdx < totalMacros; y++ {
		for x := 0; x < cols && pixelIdx < totalMacros; x++ {
			byteIdx := pixelIdx / pixelsPerByte
			if byteIdx >= len(allBytes) {
				break
			}

			var bits byte
			if f.Config.GrayLevels == 2 {
				// 1-bit encoding: 8 pixels per byte
				shift := uint(7 - (pixelIdx % 8)) // 7, 6, ..., 0
				bits = (allBytes[byteIdx] >> shift) & 0x01
			} else {
				// 2-bit encoding: 4 pixels per byte
				shift := uint(6 - (pixelIdx%4)*2) // 6, 4, 2, 0
				bits = (allBytes[byteIdx] >> shift) & 0x03
			}

			pixels[pixelIdx] = MacroPixel{
				X:        x * f.Config.MacroSize,
				Y:        y * f.Config.MacroSize,
				DataByte: bits,
				Size:     f.Config.MacroSize,
				IsBinary: f.Config.GrayLevels == 2,
			}
			pixelIdx++
		}
	}

	return pixels, nil
}

func CalculateFileHash(data []byte) [32]byte {
	return sha256.Sum256(data)
}
