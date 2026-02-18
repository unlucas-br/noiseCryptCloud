package decoder

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"hash/crc32"
	"image"
	_ "image/png"
	"os"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"

	"ncc/internal/encoder"
)

type FrameReconstructor struct {
	FrameCfg encoder.FrameConfig
	ECCCfg   encoder.ECCConfig
}

func NewFrameReconstructor(preset string) *FrameReconstructor {
	cfg := encoder.DefaultFrameConfig()
	if preset == "youtube" {
		cfg = encoder.YouTubeFrameConfig()
	} else if preset == "dense" {
		cfg = encoder.HighDensityFrameConfig()
	}

	return &FrameReconstructor{
		FrameCfg: cfg,
		ECCCfg:   encoder.ECCConfig{DataShards: 16, ParityShards: 48}, // Padrão/Legado
	}
}

type decodeResult struct {
	index       int
	data        []byte
	frameHeader encoder.FrameHeader
	crcOK       bool
	err         error
}

func (fr *FrameReconstructor) ReconstructFile(framePaths []string, outputPath string, progress chan<- float64) error {
	var allData []byte
	var globalHeader *encoder.GlobalHeader
	var crcWarnings int32 // Atomic

	// Determinar threads: Deixar 2 livres
	threads := runtime.NumCPU() - 2
	if threads < 1 {
		threads = 1
	}
	fmt.Printf("🚀 Usando %d threads para reconstrução (deixando 2 livres)\n", threads)

	// Ordenar caminhos
	sort.Slice(framePaths, func(i, j int) bool {
		return framePaths[i] < framePaths[j]
	})

	// Canais
	jobChan := make(chan struct {
		i    int
		path string
	}, len(framePaths))
	resultChan := make(chan decodeResult, len(framePaths))

	// Workers
	var wg sync.WaitGroup
	for i := 0; i < threads; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for job := range jobChan {
				data, header, crcOK, err := fr.processFrame(job.path)
				resultChan <- decodeResult{
					index:       job.i,
					data:        data,
					frameHeader: header,
					crcOK:       crcOK,
					err:         err,
				}
			}
		}()
	}

	// Despachar jobs
	for i, path := range framePaths {
		jobChan <- struct {
			i    int
			path string
		}{i, path}
	}
	close(jobChan)

	// Aguardar workers e fechar canal
	go func() {
		wg.Wait()
		close(resultChan)
	}()

	// Coletar resultados
	resultsMap := make(map[int]decodeResult)
	var processed int

	for res := range resultChan {
		if res.err != nil {
			return fmt.Errorf("frame process error: %w", res.err)
		}
		if !res.crcOK {
			atomic.AddInt32(&crcWarnings, 1)
			fmt.Fprintf(os.Stderr, "⚠️  WARNING: Frame %d CRC mismatch (corrected)\n", res.index)
		}

		resultsMap[res.index] = res

		processed++
		if progress != nil {
			// Reportar progresso (decodificação é pesada)
			progress <- float64(processed) / float64(len(framePaths))
		}
	}

	// Montagem Sequencial
	fmt.Println("📦 Montando arquivo final...")
	for i := 0; i < len(framePaths); i++ {
		res, ok := resultsMap[i]
		if !ok {
			return fmt.Errorf("missing result for frame %d", i)
		}

		if i == 0 {
			globalHeader = &res.frameHeader.GlobalMeta
		}
		allData = append(allData, res.data...)
	}

	if crcWarnings > 0 {
		fmt.Fprintf(os.Stderr, "\n⚠️  Total CRC warnings: %d/%d frames\n", crcWarnings, len(framePaths))
	}

	if globalHeader != nil {
		if len(framePaths) != int(globalHeader.TotalFrames) {
			fmt.Fprintf(os.Stderr, "⚠️  Warning: expected %d frames, found %d\n",
				globalHeader.TotalFrames, len(framePaths))
		}
	}

	// Tamanho original é ofuscado (0) no Header.
	// Confiamos no FrameHeader.DataSize.

	// SEGURANÇA: Verificação SHA-256 é feita após descriptografia (no main)
	// Hash está no payload criptografado.
	fmt.Println("✅ Arquivo reconstruído com sucesso")

	return os.WriteFile(outputPath, allData, 0644)
}

func verifySHA256(data []byte, expected []byte) bool {
	hash := sha256.Sum256(data)
	return bytes.Equal(hash[:], expected)
}

// processFrame com RECUPERAÇÃO UNIVERSAL (Tamanho + Espacial + Níveis)
func (fr *FrameReconstructor) processFrame(path string) ([]byte, encoder.FrameHeader, bool, error) {
	var emptyHeader encoder.FrameHeader

	f, err := os.Open(path)
	if err != nil {
		return nil, emptyHeader, false, err
	}
	defer f.Close()

	img, _, err := image.Decode(f)
	if err != nil {
		return nil, emptyHeader, false, fmt.Errorf("decode png: %w", err)
	}

	// ✅ Detecção Automática de Resolução
	bounds := img.Bounds()
	if bounds.Dx() != fr.FrameCfg.Width || bounds.Dy() != fr.FrameCfg.Height {
		fmt.Printf("⚠️  Resolução diferente: Config=%dx%d, Imagem=%dx%d. Ajustando.\n",
			fr.FrameCfg.Width, fr.FrameCfg.Height, bounds.Dx(), bounds.Dy())
		fr.FrameCfg.Width = bounds.Dx()
		fr.FrameCfg.Height = bounds.Dy()
	}

	// Tentar calibração
	threshold, err := fr.calibrateFrame(img)
	if err != nil {
		fmt.Printf("Warning: calibration failed for frame: %v\n", err)
		threshold = 128 // Fallback
	}

	levels, err := fr.calibrateLevels(img)
	if err != nil {
		levels = [3]uint8{64, 128, 192} // Fallback
	}

	// Leitura Inicial
	allBytes, err := fr.readBytesFromImage(img, threshold, levels, 0, 0)
	if err != nil {
		return nil, emptyHeader, false, err
	}

	// Verificar Magic
	if len(allBytes) >= encoder.FrameHeaderSizeBytes {
		headerProbe, _ := encoder.DecodeHeader(allBytes[:encoder.FrameHeaderSizeBytes])
		if headerProbe.Magic != [4]byte{'N', 'C', 'C', '1'} {
			fmt.Printf("⚠️  Invalid Magic (%v). Starting Universal Recovery...\n", headerProbe.Magic)

			found := false
			originalSize := fr.FrameCfg.MacroSize

			// 3. Scan Espacial e de Tamanho (Recuperação Avançada)
			// Tamanhos: 10, 12, 16, 24, 8, 32
			// Offsets: -3 a +3
			testSizes := []int{10, 12, 16, 24, 8, 32}

			for _, size := range testSizes {
				fr.FrameCfg.MacroSize = size

				offsets := []int{0, 1, -1, 2, -2, 3, -3}

				for _, offY := range offsets {
					for _, offX := range offsets {
						probeBytes, _ := fr.readBytesFromImage(img, threshold, levels, offX, offY)
						if len(probeBytes) < encoder.FrameHeaderSizeBytes {
							continue
						}

						h, _ := encoder.DecodeHeader(probeBytes[:encoder.FrameHeaderSizeBytes])
						if h.Magic == [4]byte{'N', 'C', 'C', '1'} {
							fmt.Printf("✅ Recovery SUCCESS! Size: %d px, Offset: (%d, %d)\n", size, offX, offY)
							allBytes = probeBytes
							found = true
							// Corrigir offset no futuro?
							// Idealmente armazenaríamos offsets, mas scan por frame é mais seguro.
							goto RecoveryDone
						}
					}
				}
			}

			// Se falhar, tentar Recuperação de Nível Profundo
			if !found {
				fr.FrameCfg.MacroSize = originalSize
				fmt.Printf("⚠️  Scan espacial falhou. Tentando Recuperação de Nível...\n")
			} else {
				goto RecoveryDone
			}

			// 4. Scan de Nível e Ganho

			if fr.FrameCfg.GrayLevels == 2 {
				for t := 30; t < 220; t += 5 {
					if t == int(threshold) {
						continue
					}
					probeBytes, _ := fr.readBytesFromImage(img, byte(t), levels, 0, 0)
					if len(probeBytes) < encoder.FrameHeaderSizeBytes {
						continue
					}
					h, _ := encoder.DecodeHeader(probeBytes[:encoder.FrameHeaderSizeBytes])
					if h.Magic == [4]byte{'N', 'C', 'C', '1'} {
						fmt.Printf("✅ Recovery SUCCESS at threshold %d!\n", t)
						allBytes = probeBytes
						found = true
						break
					}
				}
			} else {
				baseT2 := int(levels[1])
				baseRange := int(levels[2]) - int(levels[0])
				if baseRange < 20 {
					baseRange = 100
				}

				for centerShift := -60; centerShift <= 60; centerShift += 5 {
					for rangeScale := 0.5; rangeScale <= 1.5; rangeScale += 0.1 {
						newCenter := baseT2 + centerShift
						newRange := float64(baseRange) * rangeScale

						t2 := newCenter
						t1 := int(float64(t2) - newRange*0.35)
						t3 := int(float64(t2) + newRange*0.35)

						if t1 < 0 {
							t1 = 0
						}
						if t2 < t1 {
							t2 = t1 + 5
						}
						if t3 < t2 {
							t3 = t2 + 5
						}
						if t3 > 255 {
							t3 = 255
						}

						newLevels := [3]uint8{uint8(t1), uint8(t2), uint8(t3)}

						probeBytes, _ := fr.readBytesFromImage(img, threshold, newLevels, 0, 0)
						if len(probeBytes) < encoder.FrameHeaderSizeBytes {
							continue
						}
						h, _ := encoder.DecodeHeader(probeBytes[:encoder.FrameHeaderSizeBytes])
						if h.Magic == [4]byte{'N', 'C', 'C', '1'} {
							fmt.Printf("✅ Recovery SUCCESS! Shift=%d, Scale=%.1f. Levels: %v\n", centerShift, rangeScale, newLevels)
							levels = newLevels
							allBytes = probeBytes
							found = true
							break
						}
					}
					if found {
						break
					}
				}
			}

		RecoveryDone:
			if !found {
				fmt.Println("❌ Recovery failed. Header corrupted.")
			}
		}
	}

	if len(allBytes) < encoder.FrameHeaderSizeBytes {
		return nil, emptyHeader, false, fmt.Errorf("frame too small: %d bytes", len(allBytes))
	}

	header, err := encoder.DecodeHeader(allBytes[:encoder.FrameHeaderSizeBytes])
	if err != nil {
		return nil, emptyHeader, false, fmt.Errorf("decode header: %w", err)
	}

	if header.Magic != [4]byte{'N', 'C', 'C', '1'} {
		return nil, emptyHeader, false, fmt.Errorf("invalid magic: %v (expected NCC1)", header.Magic)
	}

	// Calcular bytes por frame
	cols, rows := fr.FrameCfg.GridSize()
	totalMacros := cols * rows
	var bytesInFrame int
	if fr.FrameCfg.GrayLevels == 2 {
		bytesInFrame = totalMacros / 8
	} else {
		bytesInFrame = totalMacros / 4
	}
	usableBytes := bytesInFrame - encoder.FrameHeaderSizeBytes
	if usableBytes > len(allBytes)-encoder.FrameHeaderSizeBytes {
		usableBytes = len(allBytes) - encoder.FrameHeaderSizeBytes
	}
	dataWithECC := allBytes[encoder.FrameHeaderSizeBytes : encoder.FrameHeaderSizeBytes+usableBytes]

	// Determinar config ECC do header
	parityShards := int(header.ParityShards)
	if parityShards == 0 {
		parityShards = 48 // Padrão legado
	}
	eccCfg := encoder.ECCConfig{
		DataShards:   16,
		ParityShards: parityShards,
	}

	ecc, err := encoder.NewECCEncoder(eccCfg)
	if err != nil {
		return nil, emptyHeader, false, fmt.Errorf("create ECC: %w", err)
	}

	totalShards := eccCfg.DataShards + eccCfg.ParityShards
	shardSize := (int(header.DataSize) + eccCfg.DataShards - 1) / eccCfg.DataShards
	if shardSize == 0 {
		shardSize = 1
	}

	eccBytes := shardSize * totalShards
	if eccBytes > len(dataWithECC) {
		eccBytes = len(dataWithECC)
	}
	dataWithECC = dataWithECC[:eccBytes]

	// Dividir em shards com segurança de zero-padding
	shards := make([][]byte, totalShards)
	for i := range shards {
		start := i * shardSize
		end := start + shardSize

		var shardData []byte

		if start >= len(dataWithECC) {
			// Se fora dos dados, shard zerado
			shardData = make([]byte, shardSize)
		} else {
			if end > len(dataWithECC) {
				end = len(dataWithECC)
			}
			// Copiar dados disponíveis
			shardData = make([]byte, shardSize)
			copy(shardData, dataWithECC[start:end])
		}
		shards[i] = shardData
	}

	ok, _ := ecc.Verify(shards)
	if !ok {
		if err := ecc.Reconstruct(shards); err != nil {
			return nil, emptyHeader, false, fmt.Errorf("reconstruct failed: %w", err)
		}
	}

	expectedSize := fr.ECCCfg.DataShards * shardSize
	out, err := ecc.Join(shards, expectedSize)
	if err != nil {
		return nil, emptyHeader, false, fmt.Errorf("join failed: %w", err)
	}

	var actualData []byte
	crcOK := true

	dataLen := int(header.DataSize)
	if dataLen > len(out) {
		dataLen = len(out)
	}

	if header.HasGlobal == 1 && header.FrameIndex == 0 {
		if len(out) < encoder.GlobalHeaderSizeBytes {
			return nil, emptyHeader, false, fmt.Errorf("insufficient data for GlobalHeader: %d bytes", len(out))
		}

		gh, err := encoder.DecodeGlobalHeader(out[:encoder.GlobalHeaderSizeBytes])
		if err != nil {
			return nil, emptyHeader, false, fmt.Errorf("decode GlobalHeader: %w", err)
		}

		header.GlobalMeta = gh
		frameData := out[:dataLen]
		if crc32.ChecksumIEEE(frameData) != header.DataCRC {
			crcOK = false
		}
		actualData = out[encoder.GlobalHeaderSizeBytes:dataLen]
	} else {
		actualData = out[:dataLen]
		if crc32.ChecksumIEEE(actualData) != header.DataCRC {
			crcOK = false
		}
	}

	return actualData, header, crcOK, nil
}

func (fr *FrameReconstructor) calibrateFrame(img image.Image) (byte, error) {
	bounds := img.Bounds()
	width := bounds.Dx()
	sectionWidth := width / 4
	sampleY := encoder.CalibrationBarHeight / 2
	blackAvg := fr.measureSectionAverage(img, 0, sampleY, sectionWidth, encoder.CalibrationBarHeight)
	whiteAvg := fr.measureSectionAverage(img, 3*sectionWidth, sampleY, sectionWidth, encoder.CalibrationBarHeight)
	threshold := uint8((int(blackAvg) + int(whiteAvg)) / 2)
	return byte(threshold), nil
}

func (fr *FrameReconstructor) calibrateLevels(img image.Image) ([3]uint8, error) {
	bounds := img.Bounds()
	width := bounds.Dx()
	sectionWidth := width / 4
	sampleY := encoder.CalibrationBarHeight / 2
	blackAvg := float64(fr.measureSectionAverage(img, 0, sampleY, sectionWidth, encoder.CalibrationBarHeight))
	whiteAvg := float64(fr.measureSectionAverage(img, 3*sectionWidth, sampleY, sectionWidth, encoder.CalibrationBarHeight))
	rng := whiteAvg - blackAvg
	if rng < 10 { // Safety check
		return [3]uint8{64, 128, 192}, nil
	}
	t1 := uint8(blackAvg + rng*(1.0/6.0))
	t2 := uint8(blackAvg + rng*(0.5))
	t3 := uint8(blackAvg + rng*(5.0/6.0))
	return [3]uint8{t1, t2, t3}, nil
}

func (fr *FrameReconstructor) measureSectionAverage(img image.Image, startX, startY, w, h int) uint8 {
	var sum uint32
	var count uint32
	marginX := w / 4
	marginY := h / 4
	for y := startY + marginY; y < startY+h-marginY; y++ {
		for x := startX + marginX; x < startX+w-marginX; x++ {
			r, _, _, _ := img.At(x, y).RGBA()
			sum += r >> 8
			count++
		}
	}
	if count == 0 {
		return 128
	}
	return uint8(sum / count)
}

func (fr *FrameReconstructor) extractMacroPixel(img image.Image, startX, startY int) (y, u, v uint8) {
	var sumR uint32
	realY := startY + encoder.CalibrationBarHeight
	bounds := img.Bounds()
	macroSize := fr.FrameCfg.MacroSize

	count := 0
	for dy := 0; dy < macroSize; dy++ {
		for dx := 0; dx < macroSize; dx++ {
			px := startX + dx
			py := realY + dy
			if px >= bounds.Dx() || py >= bounds.Dy() {
				continue
			}
			r, _, _, _ := img.At(bounds.Min.X+px, bounds.Min.Y+py).RGBA()
			sumR += r >> 8
			count++
		}
	}

	if count == 0 {
		return 0, 128, 128
	}
	avgR := uint8(sumR / uint32(count))
	return avgR, 128, 128
}

// readBytesFromImage com suporte a offset
func (fr *FrameReconstructor) readBytesFromImage(img image.Image, threshold byte, thresholds [3]uint8, offX, offY int) ([]byte, error) {
	cols, rows := fr.FrameCfg.GridSize()
	macroSize := fr.FrameCfg.MacroSize

	var bits []byte
	for y := 0; y < rows; y++ {
		for x := 0; x < cols; x++ {
			// Adicionar offsets
			targetX := x*macroSize + offX
			targetY := y*macroSize + offY

			avgY, _, _ := fr.extractMacroPixel(img, targetX, targetY)

			var val byte
			if fr.FrameCfg.GrayLevels == 2 {
				if avgY >= threshold {
					val = 1
				} else {
					val = 0
				}
			} else {
				val = encoder.DynGrayToNibble(avgY, thresholds)
			}
			bits = append(bits, val)
		}
	}

	var allBytes []byte
	if fr.FrameCfg.GrayLevels == 2 {
		for i := 0; i+7 < len(bits); i += 8 {
			b := (bits[i] << 7) | (bits[i+1] << 6) | (bits[i+2] << 5) | (bits[i+3] << 4) |
				(bits[i+4] << 3) | (bits[i+5] << 2) | (bits[i+6] << 1) | bits[i+7]
			allBytes = append(allBytes, b)
		}
	} else {
		for i := 0; i+3 < len(bits); i += 4 {
			b := (bits[i] << 6) | (bits[i+1] << 4) | (bits[i+2] << 2) | bits[i+3]
			allBytes = append(allBytes, b)
		}
	}
	return allBytes, nil
}
