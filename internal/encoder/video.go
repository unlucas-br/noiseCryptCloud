package encoder

import (
	"bytes"
	"fmt"
	"image"

	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"time"
)

// Constante de framer.go

type VideoEncoder struct {
	FrameCfg FrameConfig
	ECCCfg   ECCConfig
	TempDir  string
	Threads  int
	GPU      string // Opções: "none", "nvidia", "amd", "intel", "auto"
	Preset   string // Opções: "default", "fast", "youtube", "dense"
}

func NewVideoEncoder(redundancy string, threads int, preset string, gpu string) (*VideoEncoder, error) {
	tempDir, err := os.MkdirTemp("", "ncc-*")
	if err != nil {
		return nil, err
	}

	if threads <= 0 {
		threads = runtime.NumCPU() - 2
		if threads < 1 {
			threads = 1
		}
		fmt.Printf("ℹ️  Threads: %d (reservando 2 cores)\n", threads)
	}

	frameCfg := DefaultFrameConfig()
	if preset == "youtube" {
		frameCfg = YouTubeFrameConfig()
	} else if preset == "dense" {
		frameCfg = HighDensityFrameConfig()
	} else if preset == "fast" {
		frameCfg = DefaultFrameConfig() // Fast usa frame padrão mas parâmetros rápidos
	}

	return &VideoEncoder{
		FrameCfg: frameCfg,
		ECCCfg:   NewECCConfig(redundancy),
		TempDir:  tempDir,
		Threads:  threads,
		GPU:      gpu,
		Preset:   preset,
	}, nil
}

func (ve *VideoEncoder) Cleanup() {
	os.RemoveAll(ve.TempDir)
}

func (ve *VideoEncoder) EncodeFile(inputPath, outputPath string, progress chan<- float64) error {
	// Validação prévia
	info, err := os.Stat(inputPath)
	if err != nil {
		return fmt.Errorf("❌ Arquivo não encontrado: %w", err)
	}
	if info.IsDir() {
		return fmt.Errorf("❌ '%s' é um DIRETÓRIO. Compacte primeiro: zip -r %s.zip %s",
			inputPath, inputPath, inputPath)
	}

	// Determinar encoder para log
	encoderType := "CPU (libx264)"
	if ve.GPU != "none" {
		if ve.GPU == "auto" {
			encoderType = "AUTO (Procurando...)"
		} else {
			encoderType = fmt.Sprintf("GPU (%s)", ve.GPU)
		}
	}
	fmt.Printf("📊 Tamanho: %.2f MB | Threads: %d | Encoder: %s\n", float64(info.Size())/1024/1024, ve.Threads, encoderType)

	data, err := os.ReadFile(inputPath)
	if err != nil {
		return fmt.Errorf("read file: %w", err)
	}

	fileHash := CalculateFileHash(data)
	originalSize := uint64(len(data))

	// ✅ Usa constantes documentadas de framer.go
	capacityFrame0 := ve.FrameCfg.CapacityPerFrame(ve.ECCCfg, true)
	capacityOthers := ve.FrameCfg.CapacityPerFrame(ve.ECCCfg, false)

	// Cálculo do número de frames
	remainingAfterFrame0 := len(data)
	if remainingAfterFrame0 > capacityFrame0 {
		remainingAfterFrame0 -= capacityFrame0
	} else {
		remainingAfterFrame0 = 0
	}

	totalFrames := 1
	if remainingAfterFrame0 > 0 {
		totalFrames += (remainingAfterFrame0 + capacityOthers - 1) / capacityOthers
	}

	// Iniciar pipe FFmpeg
	ffmpegCmd, ffmpegStdin, err := ve.StartFFmpegPipe(outputPath, totalFrames)
	if err != nil {
		return fmt.Errorf("failed to start ffmpeg: %w", err)
	}
	defer ffmpegStdin.Close() // Fechar em erro

	// Configuração do Worker Pool
	type Job struct {
		Index int
		Data  []byte
	}
	type Result struct {
		Index  int
		Pixels []MacroPixel
		Err    error
	}

	// Limitar buffer (controle de memória)
	bufferSize := ve.Threads * 4
	if bufferSize < totalFrames {
		// Limitar se muitos frames
	} else {
		bufferSize = totalFrames
	}

	jobs := make(chan Job, bufferSize)
	results := make(chan Result, bufferSize)

	// POOL: Calcular max macro pixels
	cols, rows := ve.FrameCfg.GridSize()
	totalMacros := cols * rows
	pixelPool := sync.Pool{
		New: func() interface{} {
			// Alocar slice
			return make([]MacroPixel, totalMacros)
		},
	}

	// Iniciar Workers
	var wg sync.WaitGroup
	for w := 0; w < ve.Threads; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()

			// REUSE: Encoder ECC único por worker
			workerECC, err := NewECCEncoder(ve.ECCCfg)
			if err != nil {
				// Erro fatal
				results <- Result{Index: -1, Err: fmt.Errorf("init ecc: %w", err)}
				return
			}

			for job := range jobs {
				// REUSO: Buffer do pool
				pixelBuf := pixelPool.Get().([]MacroPixel)

				// Instância de frame separada
				frame, err := NewFrame(
					ve.FrameCfg,
					workerECC, // Encoder reutilizado
					job.Index,
					job.Data,
					totalFrames,
					originalSize,
					fileHash,
				)
				if err != nil {
					pixelPool.Put(pixelBuf) // Retornar em erro
					results <- Result{Index: job.Index, Err: err}
					return
				}

				pixels, err := frame.Render(pixelBuf) // Renderizar no buffer
				if err != nil {
					pixelPool.Put(pixelBuf) // Return on error
					results <- Result{Index: job.Index, Err: err}
					return
				}
				results <- Result{Index: job.Index, Pixels: pixels, Err: nil}
			}
		}()
	}

	// Enfileirar Jobs
	go func() {
		for i := 0; i < totalFrames; i++ {
			var frameData []byte
			if i == 0 {
				end := capacityFrame0
				if end > len(data) {
					end = len(data)
				}
				frameData = data[:end]
			} else {
				start := capacityFrame0 + (i-1)*capacityOthers
				end := start + capacityOthers
				if start >= len(data) {
					frameData = []byte{}
				} else {
					if end > len(data) {
						end = len(data)
					}
					frameData = data[start:end]
				}
			}
			jobs <- Job{Index: i, Data: frameData}
		}
		close(jobs)
		wg.Wait()
		close(results)
	}()

	// Collect Results and reorder
	// Map para frames fora de ordem
	pending := make(map[int][]MacroPixel)
	// nextFrame := 0

	nextFrameIndex := 0 // Renamed from nextFrame

	// Buffer principal para FFmpeg (apenas barra de calibração)
	calibrationImg := image.NewRGBA(image.Rect(0, 0, ve.FrameCfg.Width, ve.FrameCfg.Height))
	ve.renderCalibrationBar(calibrationImg)
	calibrationBarPix := calibrationImg.Pix[:CalibrationBarHeight*calibrationImg.Stride] // Pixels pré-renderizados

	for res := range results {
		if res.Err != nil {
			return fmt.Errorf("worker error frame %d: %w", res.Index, res.Err)
		}

		// Armazenar no mapa
		pending[res.Index] = res.Pixels

		// Processar frames pendentes
		for {
			pixels, ok := pending[nextFrameIndex]
			if !ok {
				break // Próximo frame não pronto
			}

			// Criar novo buffer de imagem
			img := image.NewRGBA(image.Rect(0, 0, ve.FrameCfg.Width, ve.FrameCfg.Height))

			// Copiar barra de calibração
			copy(img.Pix[:CalibrationBarHeight*img.Stride], calibrationBarPix)

			// Desenhar dados no buffer
			ve.drawFrameToBuffer(img, pixels)

			// Escrever no pipe FFmpeg
			if _, err := ffmpegStdin.Write(img.Pix); err != nil {
				return fmt.Errorf("write frame %d to ffmpeg: %w", nextFrameIndex, err)
			}

			// REUSO: Retornar buffer
			delete(pending, nextFrameIndex)
			pixelPool.Put(pixels)

			// Atualizar progresso
			if progress != nil {
				progress <- float64(nextFrameIndex+1) / float64(totalFrames)
			}
			nextFrameIndex++
		}
	}

	// Fechar stdin (EOF)
	ffmpegStdin.Close()

	// Aguardar finalização
	if err := ffmpegCmd.Wait(); err != nil {
		return fmt.Errorf("ffmpeg finish: %w", err)
	}

	return nil
}

// renderCalibrationBar: Desenha barra estática (Preto/Branco/Preto/Branco)
func (ve *VideoEncoder) renderCalibrationBar(img *image.RGBA) {
	width := img.Bounds().Dx()
	sectionWidth := width / 4

	for y := 0; y < CalibrationBarHeight; y++ {
		for x := 0; x < width; x++ {
			var val uint8 = 0
			// Seção 0: Preto
			// Seção 1: Branco
			if x >= sectionWidth && x < sectionWidth*2 {
				val = 255
			}
			// Seção 2: Preto
			// Seção 3: Branco
			if x >= sectionWidth*3 {
				val = 255
			}

			offset := img.PixOffset(x, y)
			img.Pix[offset] = val   // R
			img.Pix[offset+1] = val // G
			img.Pix[offset+2] = val // B
			img.Pix[offset+3] = 255 // A
		}
	}
}

// drawFrameToBuffer: Atualiza buffer com dados do frame
func (ve *VideoEncoder) drawFrameToBuffer(img *image.RGBA, pixels []MacroPixel) {
	// Nota: Barra já está no buffer

	stride := img.Stride

	for _, mp := range pixels {
		offsetY := mp.Y + CalibrationBarHeight

		// Otimização: Buffer de linha na stack (se pequeno)
		// mp.Size 16 = 64 bytes
		rowWidth := mp.Size * 4
		rowBuffer := make([]byte, rowWidth)

		gray := mp.ByteToGray()

		// Fill row buffer
		for k := 0; k < mp.Size; k++ {
			rowBuffer[k*4] = gray   // R
			rowBuffer[k*4+1] = gray // G
			rowBuffer[k*4+2] = gray // B
			rowBuffer[k*4+3] = 255  // A
		}

		// Copy row buffer to image lines
		for y := 0; y < mp.Size; y++ {
			rowStart := (offsetY + y) * stride
			pixelOffset := rowStart + mp.X*4
			copy(img.Pix[pixelOffset:pixelOffset+rowWidth], rowBuffer)
		}
	}
}

func (ve *VideoEncoder) StartFFmpegPipe(outputPath string, totalFrames int) (*exec.Cmd, io.WriteCloser, error) {
	ffmpegPath := findFFmpeg()

	// Seleção de Codec GPU
	videoCodec := "libx264" // CPU default
	gpuFlags := []string{}

	if ve.GPU == "nvidia" || ve.GPU == "nvenc" {
		videoCodec = "h264_nvenc"
		if ve.Preset == "fast" {
			gpuFlags = []string{"-preset", "p1"}
		} else {
			gpuFlags = []string{"-preset", "p7", "-tune", "hq"}
		}
	} else if ve.GPU == "amd" || ve.GPU == "amf" {
		videoCodec = "h264_amf"
		if ve.Preset == "fast" {
			gpuFlags = []string{"-quality", "speed"}
		} else {
			gpuFlags = []string{"-quality", "quality"}
		}
	} else if ve.GPU == "intel" || ve.GPU == "qsv" {
		videoCodec = "h264_qsv"
		if ve.Preset == "fast" {
			gpuFlags = []string{"-preset", "veryfast"}
		} else {
			gpuFlags = []string{"-global_quality", "20"}
		}
	} else if ve.GPU == "auto" {
		// Auto-detectar melhor GPU
		gpus := []string{"nvidia", "amd", "intel"}
		for _, g := range gpus {
			if err := VerifyGPU(g); err == nil {
				fmt.Printf("✨ GPU Detectada: %s\n", g)
				if g == "nvidia" {
					videoCodec = "h264_nvenc"
					if ve.Preset == "fast" {
						gpuFlags = []string{"-preset", "p1"} // Max Speed
					} else {
						gpuFlags = []string{"-preset", "p7", "-tune", "hq"}
					}
				} else if g == "amd" {
					videoCodec = "h264_amf"
					if ve.Preset == "fast" {
						gpuFlags = []string{"-quality", "speed"}
					} else {
						gpuFlags = []string{"-quality", "quality"}
					}
				} else if g == "intel" {
					videoCodec = "h264_qsv"
					if ve.Preset == "fast" {
						gpuFlags = []string{"-preset", "veryfast"}
					} else {
						gpuFlags = []string{"-global_quality", "20"}
					}
				}
				break
			}
		}
		if videoCodec == "libx264" {
			fmt.Println("⚠️  Nenhum encoder de GPU encontrado. Usando CPU.")
		}
	}

	args := []string{
		"-y",
		"-f", "rawvideo",
		"-pixel_format", "rgba",
		"-video_size", fmt.Sprintf("%dx%d", ve.FrameCfg.Width, ve.FrameCfg.Height),
		"-framerate", fmt.Sprintf("%d", ve.FrameCfg.FPS),
		"-i", "pipe:0",
		"-c:v", videoCodec,
	}

	if videoCodec == "libx264" {
		if ve.Preset == "fast" {
			args = append(args, "-preset", "ultrafast", "-crf", "23")
		} else {
			args = append(args, "-preset", "slow", "-crf", "23")
		}
	} else {
		// Flags específicas de GPU
		args = append(args, gpuFlags...)
		// Fallback para bitrate fixo em GPUs sem suporte CRF
		if videoCodec == "h264_nvenc" {
			args = append(args, "-cq", "24")
		} else {
			args = append(args, "-b:v", "5M") // 5Mbps target
		}
	}

	args = append(args,
		"-pix_fmt", "yuv420p",
		"-movflags", "+faststart",
		outputPath,
	)

	cmd := exec.Command(ffmpegPath, args...)
	// Suprimir output, exceto debug
	// cmd.Stderr = os.Stderr

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, nil, fmt.Errorf("stdin pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		stdin.Close()
		return nil, nil, fmt.Errorf("start ffmpeg: %w", err)
	}

	return cmd, stdin, nil
}

// findFFmpeg: Busca FFmpeg no PATH e locais comuns
func findFFmpeg() string {
	// Tentar PATH
	if path, err := exec.LookPath("ffmpeg"); err == nil {
		return path
	}

	// Locais comuns Windows
	locations := []string{
		`C:\ffmpeg\bin\ffmpeg.exe`,
		`C:\Program Files\ffmpeg\bin\ffmpeg.exe`,
		`C:\Program Files (x86)\ffmpeg\bin\ffmpeg.exe`,
		filepath.Join(os.Getenv("LOCALAPPDATA"), "Microsoft", "WinGet", "Links", "ffmpeg.exe"),
		filepath.Join(os.Getenv("USERPROFILE"), "scoop", "shims", "ffmpeg.exe"),
	}

	for _, loc := range locations {
		if _, err := os.Stat(loc); err == nil {
			return loc
		}
	}

	// Fallback para 'ffmpeg'
	return "ffmpeg"
}

// VerifyGPU: Verifica se o encoder GPU solicitado está funcional
func VerifyGPU(gpuType string) error {
	ffmpegPath := findFFmpeg()

	codec := ""
	if gpuType == "nvidia" {
		codec = "h264_nvenc"
	} else if gpuType == "amd" {
		codec = "h264_amf"
	} else if gpuType == "intel" {
		codec = "h264_qsv"
	} else {
		return fmt.Errorf("unknown gpu type: %s", gpuType)
	}

	// Testar encode de 1 frame
	// ffmpeg -y -hide_banner -f lavfi -i color=c=black:s=64x64 -vframes 1 -c:v h264_nvenc -f null -
	cmd := exec.Command(ffmpegPath,
		"-y",
		"-hide_banner",
		"-f", "lavfi",
		"-i", "color=c=black:s=256x256",
		"-vframes", "1",
		"-c:v", codec,
		"-f", "null",
		"-",
	)

	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("GPU check failed for '%s' (%s):\n%s", gpuType, codec, string(output))
	}
	return nil
}

// BenchmarkSpeed: Teste curto de encode para medir FPS
func BenchmarkSpeed(gpuType string, width, height, fps int) (float64, error) {
	ffmpegPath := findFFmpeg()
	codec := "libx264"
	args := []string{}

	if gpuType != "none" {
		if gpuType == "nvidia" {
			codec = "h264_nvenc"
			args = append(args, "-preset", "p7", "-tune", "hq")
		} else if gpuType == "amd" {
			codec = "h264_amf"
			args = append(args, "-quality", "speed")
		} else if gpuType == "intel" {
			codec = "h264_qsv"
			args = append(args, "-global_quality", "20")
		} else {
			return 0, fmt.Errorf("unknown gpu type: %s", gpuType)
		}
	} else {
		args = append(args, "-preset", "ultrafast")
		args = append(args, "-preset", "slow", "-crf", "23")
	}

	// Gerar 5s de vídeo para teste
	// ffmpeg -f lavfi -i nullsrc=s=1280x720 -t 5 -c:v libx264 -f null -
	cmd := exec.Command(ffmpegPath,
		"-y",
		"-hide_banner",
		"-f", "lavfi",
		"-i", fmt.Sprintf("testsrc=size=%dx%d:rate=%d", width, height, fps),
		"-t", "5", // 5 seconds
		"-c:v", codec,
	)
	cmd.Args = append(cmd.Args, args...)
	cmd.Args = append(cmd.Args, "-f", "null", "-")

	// Extrair FPS do stderr
	// We need to capture stderr and parse the last line 'fps=...'
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	start := time.Now()
	if err := cmd.Run(); err != nil {
		return 0, fmt.Errorf("benchmark failed for %s: %w", gpuType, err)
	}
	duration := time.Since(start)

	totalFrames := 5 * fps
	calculatedFPS := float64(totalFrames) / duration.Seconds()

	return calculatedFPS, nil
}
