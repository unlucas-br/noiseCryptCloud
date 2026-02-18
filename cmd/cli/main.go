package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"ncc/internal/cluster"
	"ncc/internal/crypto"
	"ncc/internal/decoder"
	"ncc/internal/encoder"
)

func main() {
	var (
		mode       = flag.String("mode", "", "Modo: encode, decode, master, worker")
		input      = flag.String("input", "", "Arquivo de entrada")
		output     = flag.String("output", "", "Arquivo de saída")
		password   = flag.String("password", "", "Senha de criptografia (opcional)")
		redundancy = flag.String("redundancy", "medium", "Nível de redundância: low, medium, high")
		threads    = flag.Int("threads", 0, "Número de threads (0 = auto)")
		preset     = flag.String("preset", "default", "Preset: default, fast, youtube")
		gpu        = flag.String("gpu", "auto", "Aceleração GPU: auto, nvidia, amd, intel, none")
		masterPort = flag.Int("port", 9090, "Porta do servidor Master")
		masterURL  = flag.String("master", "", "URL do Master (modo worker)")
	)
	flag.Parse()

	if *mode == "" || (*mode != "check" && *mode != "worker" && *input == "") {
		fmt.Println("╔══════════════════════════════════════╗")
		fmt.Println("║         noiseCryptCloud (ncc)        ║")
		fmt.Println("╚══════════════════════════════════════╝")
		fmt.Println("Uso:")
		fmt.Println("  ncc -mode=encode -input=arquivo.any -output=arquivo_ncc.mp4 -preset=fast")
		fmt.Println("  ncc -mode=encode -input=arquivo.any -password=senha123 -preset=fast")
		fmt.Println("  ncc -mode=decode -input=arquivo_ncc.mp4 -output=recuperado.any -preset=fast")
		fmt.Println("  ncc -mode=master -input=arquivo.any -password=senha123 -preset=fast -port=9090")
		fmt.Println("  ncc -mode=worker -master=\"http://localhost:9090\"")
		fmt.Println()
		fmt.Println("Opções:")
		fmt.Println("  -mode:           'encode', 'decode', 'master', 'worker'")
		fmt.Println("  -input:          Arquivo de entrada (obrigatório para encode/decode/master)")
		fmt.Println("  -output:         Arquivo de saída (opcional)")
		fmt.Println("  -password:       Senha de criptografia")
		fmt.Println("  -redundancy:     'low', 'medium' (padrão), 'high'")
		fmt.Println("  -threads:        Threads (0 = auto)")
		fmt.Println("  -preset:         'default', 'fast', 'youtube'")
		fmt.Println("  -gpu:            'auto', 'nvidia', 'amd', 'intel', 'none'")
		fmt.Println("  -port:           Porta do Master")
		fmt.Println("  -master:         URL do Master")
		fmt.Println()
		fmt.Println("⚠️  Cluster (master/worker): Execute antes o comando:")
		fmt.Println("   cloudflared tunnel --url http://localhost:9090")
		os.Exit(1)
	}

	if *output == "" && *mode != "worker" {
		if *mode == "encode" || *mode == "master" {
			*output = strings.TrimSuffix(*input, filepath.Ext(*input)) + "_ncc.mp4"
		} else {
			*output = strings.TrimSuffix(*input, filepath.Ext(*input)) + "_recovered.bin"
		}
	}

	fmt.Println("╔══════════════════════════════════════╗")
	fmt.Println("║         noiseCryptCloud (ncc)        ║")
	fmt.Println("╚══════════════════════════════════════╝")
	if *mode != "worker" {
		fmt.Println("Iniciando análise...")
		fmt.Printf("Modo:    %s\n", *mode)
		fmt.Printf("Entrada: %s\n", *input)
		fmt.Printf("Saída:   %s\n", *output)
		fmt.Println()
	}

	var err error
	if *mode == "encode" {
		err = runEncode(*input, *output, *password, *redundancy, *threads, *preset, *gpu)
	} else if *mode == "decode" {
		err = runDecode(*input, *output, *password, *preset)
	} else if *mode == "analyze" {
		err = runAnalyze(*input, *password, *redundancy, *preset)
	} else if *mode == "check" {
		err = runCheck(*gpu)
	} else if *mode == "master" {
		err = runMaster(*input, *output, *password, *redundancy, *threads, *preset, *gpu, *masterPort)
	} else if *mode == "worker" {
		err = runWorker(*masterURL, *threads)
	} else {
		fmt.Printf("❌ Modo inválido: %s (use 'encode', 'decode', 'master' ou 'worker')\n", *mode)
		os.Exit(1)
	}

	if err != nil {
		fmt.Printf("❌ Error: %v\n", err)
		os.Exit(1)
	}

	fmt.Println()
	fmt.Println("✅ Done!")
}

func runEncode(inputPath, outputPath, password, redundancy string, threads int, preset string, gpu string) error {
	// Validate input
	info, err := os.Stat(inputPath)
	if err != nil {
		return fmt.Errorf("file not found: %s", inputPath)
	}
	if info.IsDir() {
		return fmt.Errorf("'%s' is a directory, not a file", inputPath)
	}

	fmt.Printf("Lendo arquivo (%.2f MB)...\n", float64(info.Size())/1024/1024)

	data, err := os.ReadFile(inputPath)
	if err != nil {
		return fmt.Errorf("read file: %w", err)
	}

	// Compressão antes da criptografia
	fmt.Println("Comprimindo dados (Gzip)...")
	data, err = compressData(data)
	if err != nil {
		return fmt.Errorf("erro compressão: %w", err)
	}
	fmt.Printf("Tamanho comprimido: %d bytes\n", len(data))

	// Criptografia se senha fornecida
	if password != "" {
		fmt.Println("Criptografando...")
		data, err = crypto.EncryptWithHash(data, password)
		if err != nil {
			return fmt.Errorf("erro criptografia: %w", err)
		}
	}

	fmt.Printf("Codificando %d bytes para vídeo...\n", len(data))

	// Auto-seleção de GPU via Benchmark
	if gpu == "auto" {
		fmt.Println("Testando velocidade do hardware (~5s)...")

		// 1. Benchmark CPU
		cpuFPS, err := encoder.BenchmarkSpeed("none", 1920, 1080, 30) // Test at 1080p30
		if err != nil {
			fmt.Printf("Erro no benchmark de CPU: %v\n", err)
			cpuFPS = 0
		} else {
			fmt.Printf("   - CPU: %.1f FPS\n", cpuFPS)
		}

		// 2. Benchmark GPU (Encontrar melhor)
		bestGPU := "none"
		gpuFPS := 0.0

		// Sondar GPU
		candidates := []string{"nvidia", "amd", "intel"}
		for _, g := range candidates {
			if err := encoder.VerifyGPU(g); err == nil {
				bestGPU = g
				// Medir velocidade específica
				fps, err := encoder.BenchmarkSpeed(g, 1920, 1080, 30)
				if err != nil {
					fmt.Printf("Erro no benchmark de GPU (%s): %v\n", g, err)
				} else {
					gpuFPS = fps
					fmt.Printf("   - GPU (%s): %.1f FPS\n", g, gpuFPS)
				}
				// Parar na primeira válida
				break
			}
		}

		// 3. Lógica de seleção
		if bestGPU != "none" {
			if gpuFPS > cpuFPS {
				gpu = bestGPU
				ratio := gpuFPS / cpuFPS
				if cpuFPS == 0 {
					ratio = 999
				}
				fmt.Printf("✅ GPU selecionada (%s) - %.1fx mais rápida\n", bestGPU, ratio)
			} else {
				gpu = "none"
				ratio := cpuFPS / gpuFPS
				if gpuFPS == 0 {
					ratio = 999
				}
				fmt.Printf("✅ CPU selecionada - %.1fx mais rápida\n", ratio)
			}
		} else {
			fmt.Println("Nenhuma GPU disponível. Usando CPU.")
			gpu = "none"
		}
		fmt.Println()
	}

	// Criar encoder (com GPU definida)
	enc, err := encoder.NewVideoEncoder(redundancy, threads, preset, gpu)
	if err != nil {
		return fmt.Errorf("create encoder: %w", err)
	}
	defer enc.Cleanup()

	// Escrever dados (brutos/cifrados) em temp
	tmpFile, err := os.CreateTemp("", "ncc-*.bin")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	tmpPath := tmpFile.Name()
	defer os.Remove(tmpPath)

	if _, err := tmpFile.Write(data); err != nil {
		tmpFile.Close()
		return fmt.Errorf("write temp: %w", err)
	}
	tmpFile.Close()

	// Encode com callback de progresso
	progressCh := make(chan float64, 100)
	done := make(chan error, 1)

	go func() {
		done <- enc.EncodeFile(tmpPath, outputPath, progressCh)
		close(progressCh)
	}()

	// Exibir progresso
	startTime := time.Now()
	lastUpdate := time.Now()

	fmt.Print("\033[?25l")       // Hide cursor
	defer fmt.Print("\033[?25h") // Show cursor

	for p := range progressCh {
		// Atualizar a cada 100ms ou 1%
		if time.Since(lastUpdate) < 100*time.Millisecond && p < 1.0 {
			continue
		}

		percent := int(p * 100)

		// Calcular ETA
		elapsed := time.Since(startTime)
		if p > 0.001 { // Evitar divisão por zero
			estimatedTotal := time.Duration(float64(elapsed) / p)
			remaining := estimatedTotal - elapsed

			// Formatar duração
			remainingText := remaining.Round(time.Second).String()

			// Limpar linha e exibir status
			fmt.Printf("\rProgresso: %3d%% [ETA: %s]   ", percent, remainingText)
		} else {
			fmt.Printf("\rProgresso: %3d%% [Calculando...]   ", percent)
		}

		lastUpdate = time.Now()
	}
	fmt.Println() // New line after loop

	err = <-done
	if err != nil {
		return fmt.Errorf("encode: %w", err)
	}

	fmt.Printf("Vídeo salvo: %s\n", outputPath)
	return nil
}

func runDecode(inputPath, outputPath, password, preset string) error {
	// Validate input
	if _, err := os.Stat(inputPath); err != nil {
		return fmt.Errorf("file not found: %s", inputPath)
	}

	fmt.Println("Extraindo frames do vídeo...")
	fmt.Printf("Preset de Decode: '%s'\n", preset)

	// Criar extrator
	extractor, err := decoder.NewFrameExtractor(preset)
	if err != nil {
		return fmt.Errorf("create extractor: %w", err)
	}
	defer extractor.Cleanup()

	// Extrair frames (stderr do ffmpeg é herdado)
	frames, err := extractor.ExtractFrames(inputPath, nil)
	if err != nil {
		return fmt.Errorf("extrair frames: %w", err)
	}

	fmt.Printf("Extraídos %d frames\n", len(frames))
	fmt.Println("Reconstruindo arquivo...")

	// Reconstruir
	recon := decoder.NewFrameReconstructor(preset)
	err = recon.ReconstructFile(frames, outputPath, nil)
	if err != nil {
		return fmt.Errorf("reconstruct: %w", err)
	}

	// Descriptografar se houver senha
	// SEGURANÇA: DecryptWithHash verifica integridade via HMAC
	if password != "" {
		fmt.Println("Decriptando...")
		data, err := os.ReadFile(outputPath)
		if err != nil {
			return fmt.Errorf("read output: %w", err)
		}

		// Decriptar e verificar integridade
		decrypted, err := crypto.DecryptWithHash(data, password)
		if err != nil {
			return fmt.Errorf("decrypt: %w", err)
		}

		fmt.Println("✅ Integrity verified (HMAC-SHA256 authenticated)")

		// Descompressão
		fmt.Println("Decompressing data...")
		gz, err := gzip.NewReader(bytes.NewReader(decrypted))
		if err != nil {
			return fmt.Errorf("decompress init: %w", err)
		}

		decompressed, err := io.ReadAll(gz)
		if err != nil {
			return fmt.Errorf("decompress read: %w", err)
		}
		gz.Close()

		err = os.WriteFile(outputPath, decompressed, 0644)
		if err != nil {
			return fmt.Errorf("salvar arquivo final: %w", err)
		}
	} else {
		// Sem senha: Apenas descomprimir (se não cifrado)
		fmt.Println("Descomprimindo (sem senha)...")
		data, err := os.ReadFile(outputPath)
		if err != nil {
			return fmt.Errorf("read output: %w", err)
		}

		gz, err := gzip.NewReader(bytes.NewReader(data))
		if err != nil {
			return fmt.Errorf("decompress init: %w", err)
		}

		decompressed, err := io.ReadAll(gz)
		if err != nil {
			return fmt.Errorf("decompress read: %w", err)
		}
		gz.Close()

		err = os.WriteFile(outputPath, decompressed, 0644)
		if err != nil {
			return fmt.Errorf("salvar arquivo final: %w", err)
		}
	}

	fmt.Printf("Arquivo recuperado: %s\n", outputPath)
	return nil
}

func runAnalyze(inputPath, password, redundancy, preset string) error {
	fmt.Println("Analisando consistência do arquivo...")

	// 1. Ler Entrada
	data, err := os.ReadFile(inputPath)
	if err != nil {
		return fmt.Errorf("ler entrada: %w", err)
	}
	originalSize := len(data)
	fmt.Printf("Tamanho Original: %d bytes\n", originalSize)

	// 2. Encode para Vídeo Temp
	tmpVideo := "analyze_temp.avi"
	defer os.Remove(tmpVideo)

	fmt.Println("Codificando teste de loopback...")
	err = runEncode(inputPath, tmpVideo, password, redundancy, 0, "default", "none")
	if err != nil {
		return fmt.Errorf("falha no encode: %w", err)
	}

	// 3. Extrair Frames
	fmt.Println("Extraindo frames para verificar headers...")
	ext, err := decoder.NewFrameExtractor(preset)
	if err != nil {
		return err
	}
	defer ext.Cleanup()

	frames, err := ext.ExtractFrames(tmpVideo, nil)
	if err != nil {
		return fmt.Errorf("falha na extração: %w", err)
	}

	// 4. Analisar Frames
	recon := decoder.NewFrameReconstructor("default")

	fmt.Println("Verificando reconstrução completa...")

	tmpOutput := "analyze_output.bin"
	defer os.Remove(tmpOutput)

	err = recon.ReconstructFile(frames, tmpOutput, nil)
	if err != nil {
		fmt.Printf("❌ FALHA na reconstrução: %v\n", err)
	} else {
		// Verificar tamanho
		outData, _ := os.ReadFile(tmpOutput)
		fmt.Printf("Tamanho Final: %d bytes\n", len(outData))

		if len(outData) != originalSize {
			fmt.Printf("❌ DIFERENÇA DE TAMANHO! Diff: %d bytes\n", len(outData)-originalSize)
			if len(outData) > originalSize {
				fmt.Println("⚠️  Saída é MAIOR. Isso implica que o padding não foi removido.")
				fmt.Println("    Causa provável: Frame 0 (GlobalHeader) perdido/corrompido, OriginalSize desconhecido.")
			}
		} else {
			if bytes.Equal(data, outData) {
				fmt.Println("✅ Dados CORRESPONDEM perfeitamente.")
			} else {
				fmt.Println("❌ CONTEÚDO DIFERENTE (corrupção).")
			}
		}
	}

	return nil
}

func runCheck(gpu string) error {
	fmt.Println("Verificando capacidades do sistema...")

	// 1. Verificar FFmpeg
	cmd := exec.Command("ffmpeg", "-version")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("ffmpeg não encontrado ou erro: %v", err)
	}
	firstLine := strings.Split(string(out), "\n")[0]
	fmt.Printf("✅ FFmpeg encontrado: %s\n", firstLine)

	// 2. Verificar GPU
	if gpu == "none" || gpu == "" {
		fmt.Println("Pulei verificação de GPU (use -gpu=nvidia/amd/intel/auto para checar)")
	} else {
		fmt.Printf("Testando suporte a GPU: %s...\n", gpu)
		if gpu == "auto" {
			// Testar todas
			gpus := []string{"nvidia", "amd", "intel"}
			found := false
			for _, g := range gpus {
				fmt.Printf("  - Testando %s... ", g)
				if err := encoder.VerifyGPU(g); err == nil {
					fmt.Printf("✅ SUPORTADO!\n")
					found = true
				} else {
					fmt.Printf("❌ Indisponível\n")
				}
			}
			if !found {
				return fmt.Errorf("nenhum encoder de GPU suportado encontrado via 'auto'")
			}
		} else {
			if err := encoder.VerifyGPU(gpu); err != nil {
				return fmt.Errorf("❌ Verificação de GPU falhou:\n%v", err)
			}
			fmt.Printf("✅ GPU Verificada! (%s está funcionando)\n", gpu)
		}
	}
	return nil
}

func runMaster(inputPath, outputPath, password, redundancy string, threads int, preset string, gpu string, port int) error {
	// Validate input
	info, err := os.Stat(inputPath)
	if err != nil {
		return fmt.Errorf("file not found: %s", inputPath)
	}
	if info.IsDir() {
		return fmt.Errorf("'%s' is a directory, not a file", inputPath)
	}

	if outputPath == "" {
		outputPath = strings.TrimSuffix(inputPath, filepath.Ext(inputPath)) + "_ncc.mp4"
	}

	fmt.Println("╔══════════════════════════════════════╗")
	fmt.Println("║    noiseCryptCloud - Master Mode     ║")
	fmt.Println("╚══════════════════════════════════════╝")
	fmt.Printf("📊 File: %s (%.2f MB)\n", inputPath, float64(info.Size())/1024/1024)
	fmt.Printf("📊 Output: %s\n", outputPath)
	fmt.Printf("📊 Port: %d\n", port)
	fmt.Println()

	data, err := os.ReadFile(inputPath)
	if err != nil {
		return fmt.Errorf("read file: %w", err)
	}

	// Compressão (Master)
	fmt.Println("📦 Comprimindo dados (Gzip)...")
	data, err = compressData(data)
	if err != nil {
		return fmt.Errorf("erro compressão: %w", err)
	}
	fmt.Printf("📦 Tamanho comprimido: %d bytes\n", len(data))

	// Criptografia (no Master)
	if password != "" {
		fmt.Println("🔐 Criptografando...")
		data, err = crypto.EncryptWithHash(data, password)
		if err != nil {
			return fmt.Errorf("erro criptografia: %w", err)
		}
	}

	// Criar encoder
	enc, err := encoder.NewVideoEncoder(redundancy, threads, preset, gpu)
	if err != nil {
		return fmt.Errorf("create encoder: %w", err)
	}
	defer enc.Cleanup()

	fileHash := encoder.CalculateFileHash(data)
	originalSize := uint64(len(data))

	capacityFrame0 := enc.FrameCfg.CapacityPerFrame(enc.ECCCfg, true)
	capacityOthers := enc.FrameCfg.CapacityPerFrame(enc.ECCCfg, false)

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

	fmt.Printf("📊 Total frames: %d | Capacity: frame0=%d, others=%d bytes\n",
		totalFrames, capacityFrame0, capacityOthers)

	// Criar master
	master := cluster.NewMaster(port, enc.FrameCfg, enc.ECCCfg, totalFrames, originalSize, fileHash)

	// Adicionar jobs na fila
	for i := 0; i < totalFrames; i++ {
		var frameData []byte
		if i == 0 {
			end := capacityFrame0
			if end > len(data) {
				end = len(data)
			}
			frameData = make([]byte, end)
			copy(frameData, data[:end])
		} else {
			start := capacityFrame0 + (i-1)*capacityOthers
			end := start + capacityOthers
			if start >= len(data) {
				frameData = []byte{}
			} else {
				if end > len(data) {
					end = len(data)
				}
				frameData = make([]byte, end-start)
				copy(frameData, data[start:end])
			}
		}
		master.AddJob(cluster.FrameJob{
			FrameIndex: i,
			Data:       frameData,
		})
	}
	master.FinishAddingJobs()

	// Start HTTP server in background
	// Iniciar servidor em background
	master.StartAsync()

	// Aguardar Enter
	fmt.Println()
	fmt.Println("⏳ Aguardando workers...")
	fmt.Println("   Use em outro terminal ou máquina:")
	fmt.Printf("   ncc -mode=worker -master=\"http://localhost:%d\"\n", port)
	fmt.Println()
	fmt.Println("   Para Cloudflare Tunnel:")
	fmt.Printf("   cloudflared tunnel --url http://localhost:%d\n", port)
	fmt.Println("   Depois use a URL https:// no worker remoto")
	fmt.Println()
	fmt.Println("   Pressione ENTER para iniciar a distribuição de jobs...")
	fmt.Println("   (Conecte seus workers antes de apertar ENTER)")

	// Leitura do Enter
	var b [1]byte
	os.Stdin.Read(b[:])

	fmt.Println("🚀 Iniciando distribuição de jobs!")
	master.StartDistribution()

	// Coletar resultados e gravar no FFmpeg
	// Iniciar montagem final com FFmpeg
	ffmpegCmd, ffmpegStdin, err := enc.StartFFmpegPipe(outputPath, totalFrames)
	if err != nil {
		return fmt.Errorf("failed to start ffmpeg: %w", err)
	}

	// Buffer de escrita (4MB) para performance
	bufferedStdin := bufio.NewWriterSize(ffmpegStdin, 4*1024*1024)

	startTime := time.Now()
	pending := make(map[int][]byte) // Mapa: frameIndex -> pixels comprimidos
	nextFrameIndex := 0

	for completed := 0; completed < totalFrames; {
		result := <-master.Results
		if result.Error != "" {
			return fmt.Errorf("worker error frame %d: %s", result.FrameIndex, result.Error)
		}

		pending[result.FrameIndex] = result.CompressedPixels
		completed++

		// Gravar frames na ordem
		for {
			compressedPixels, ok := pending[nextFrameIndex]
			if !ok {
				break
			}

			// Descomprimir pixels
			pixelData, err := cluster.DecompressPixels(compressedPixels)
			if err != nil {
				return fmt.Errorf("decompress frame %d: %w", nextFrameIndex, err)
			}

			// Escrever no FFmpeg
			if _, err := bufferedStdin.Write(pixelData); err != nil {
				return fmt.Errorf("write frame %d to ffmpeg: %w", nextFrameIndex, err)
			}

			delete(pending, nextFrameIndex)

			// Progresso
			elapsed := time.Since(startTime)
			pct := float64(nextFrameIndex+1) / float64(totalFrames) * 100

			// FPS médio
			avgFps := float64(nextFrameIndex+1) / elapsed.Seconds()

			// ETA
			remainingFrames := totalFrames - (nextFrameIndex + 1)
			etaSeconds := float64(remainingFrames) / avgFps
			etaDuration := time.Duration(etaSeconds) * time.Second

			fmt.Printf("\r🚀 Progresso: %.1f%% (%d/%d) | FPS: %.1f | ETA: %v   ",
				pct, nextFrameIndex+1, totalFrames, avgFps, etaDuration.Round(time.Second))

			nextFrameIndex++
		}
	}

	fmt.Println()

	// Finalizar FFmpeg
	bufferedStdin.Flush()
	ffmpegStdin.Close()
	if err := ffmpegCmd.Wait(); err != nil {
		return fmt.Errorf("ffmpeg finish: %w", err)
	}

	elapsed := time.Since(startTime)
	fmt.Printf("🏁 Encoding completo em %v (%.1f fps média)\n", elapsed.Round(time.Second),
		float64(totalFrames)/elapsed.Seconds())
	fmt.Printf("📁 Vídeo salvo: %s\n", outputPath)

	return nil
}

func runWorker(masterURL string, threads int) error {
	if masterURL == "" {
		return fmt.Errorf("❌ URL do master não fornecida. Use: -master=\"http://localhost:9090\"")
	}

	fmt.Println("╔══════════════════════════════════════╗")
	fmt.Println("║    noiseCryptCloud - Worker Mode     ║")
	fmt.Println("╚══════════════════════════════════════╝")
	fmt.Printf("🔌 Master URL: %s\n", masterURL)
	fmt.Printf("🧵 Threads: %d\n", threads)
	fmt.Println()

	worker := cluster.NewWorker(masterURL, threads)
	return worker.Run()
}
func compressData(data []byte) ([]byte, error) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if _, err := gz.Write(data); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func decompressData(data []byte) ([]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	defer gz.Close()
	return io.ReadAll(gz)
}
