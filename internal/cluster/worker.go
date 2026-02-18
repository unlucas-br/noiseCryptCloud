package cluster

import (
	"bytes"
	"fmt"
	"image"
	"io"
	"log"
	"net/http"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"ncc/internal/encoder"
)

// Worker: Processa frames recebidos do Master via HTTP
type Worker struct {
	MasterURL string
	Threads   int
	config    JobConfig
	frameCfg  encoder.FrameConfig
	eccCfg    encoder.ECCConfig
	client    *http.Client

	// Stats
	processed atomic.Int64
}

// NewWorker cria cliente worker
func NewWorker(masterURL string, threads int) *Worker {
	if threads <= 0 {
		threads = runtime.NumCPU()
	}
	return &Worker{
		MasterURL: masterURL,
		Threads:   threads,
		client: &http.Client{
			// Timeout curto, lógica trata retentativas
			Timeout: 60 * time.Second,
		},
	}
}

// Run: Conecta ao master e inicia pipeline
func (w *Worker) Run() error {
	// 1. Buscar config
	if err := w.fetchConfig(); err != nil {
		return err
	}

	// 2. Registrar
	w.register()

	// 3. Iniciar Pipeline
	// Canais
	jobChan := make(chan FrameJob, BatchSize*2)       // Buffer 2 lotes
	resultChan := make(chan FrameResult, BatchSize*2) // Buffer 2 lotes

	// Contexto para shutdown (atomic bool)
	var stop atomic.Bool
	var wg sync.WaitGroup

	// Iniciar Busca
	wg.Add(1)
	go w.fetchLoop(jobChan, &stop, &wg)

	// Iniciar Processadores
	for i := 0; i < w.Threads; i++ {
		wg.Add(1)
		go w.processLoop(i, jobChan, resultChan, &wg)
	}

	// Iniciar Envio
	wg.Add(1)
	go w.sendLoop(resultChan, &stop, &wg)

	// Iniciar Monitor
	go w.monitorLoop(&stop)

	// Aguardar finalização
	wg.Wait()

	fmt.Println("\n✅ Work completed.")
	return nil
}

func (w *Worker) fetchConfig() error {
	fmt.Printf("🔌 Connecting to master: %s\n", w.MasterURL)
	configData, err := w.httpGet("/config")
	if err != nil {
		return fmt.Errorf("fetch config: %w", err)
	}
	if err := DecodeJSON(configData, &w.config); err != nil {
		return fmt.Errorf("decode config: %w", err)
	}

	w.frameCfg = encoder.FrameConfig{
		Width:             w.config.Width,
		Height:            w.config.Height,
		MacroSize:         w.config.MacroSize,
		FPS:               w.config.FPS,
		CalibrationHeight: w.config.CalibrationHeight,
		GrayLevels:        w.config.GrayLevels,
	}
	w.eccCfg = encoder.ECCConfig{
		DataShards:   w.config.DataShards,
		ParityShards: w.config.ParityShards,
	}

	fmt.Printf("✅ Connected! Job: %dx%d, Total frames: %d\n", w.config.Width, w.config.Height, w.config.TotalFrames)
	fmt.Printf("🧵 Threads: %d | Batch Size: %d\n", w.Threads, BatchSize)
	return nil
}

func (w *Worker) register() {
	hostname, _ := os.Hostname()
	info := WorkerInfo{
		Hostname: hostname,
		CPUCores: runtime.NumCPU(),
		OS:       runtime.GOOS,
		Arch:     runtime.GOARCH,
	}
	data, _ := EncodeJSON(info)
	w.httpPost("/register", data)
}

// fetchLoop: Busca batches continuamente
func (w *Worker) fetchLoop(jobChan chan<- FrameJob, stop *atomic.Bool, wg *sync.WaitGroup) {
	defer wg.Done()
	defer close(jobChan)

	retries := 0

	for !stop.Load() {
		// Controle de fluxo: se jobChan cheio, aguarde
		if len(jobChan) > cap(jobChan)-BatchSize {
			time.Sleep(100 * time.Millisecond)
			continue
		}

		resp, err := w.client.Get(w.MasterURL + "/batch")
		if err != nil {
			retries++
			if retries > 10 {
				log.Printf("⚠️ Too many fetch errors, stopping.")
				stop.Store(true)
				return
			}
			time.Sleep(time.Duration(retries) * time.Second)
			continue
		}

		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode == http.StatusNoContent {
			// Tudo pronto
			stop.Store(true)
			return
		} else if resp.StatusCode == http.StatusAccepted {
			// Master ocupado ou carregando
			time.Sleep(500 * time.Millisecond)
			continue
		} else if resp.StatusCode != http.StatusOK {
			log.Printf("⚠️ Fetch status %d", resp.StatusCode)
			time.Sleep(1 * time.Second)
			continue
		}

		retries = 0
		var batch []FrameJob
		if err := DecodeGob(body, &batch); err != nil {
			log.Printf("⚠️ Decode batch error: %v", err)
			continue
		}

		// Enviar ao canal
		for _, job := range batch {
			jobChan <- job
		}
	}
}

// processLoop: Consome jobs e gera resultados
func (w *Worker) processLoop(id int, jobChan <-chan FrameJob, resultChan chan<- FrameResult, wg *sync.WaitGroup) {
	defer wg.Done()

	ecc, err := encoder.NewECCEncoder(w.eccCfg)
	if err != nil {
		log.Printf("Worker %d ECC init error: %v", id, err)
		return
	}

	// Buffer de imagem reutilizável
	img := image.NewRGBA(image.Rect(0, 0, w.frameCfg.Width, w.frameCfg.Height))

	for job := range jobChan {
		result := w.processFrame(job, ecc, img)
		resultChan <- result
		w.processed.Add(1)
	}
}

// sendLoop: Coleta resultados e envia em batches
func (w *Worker) sendLoop(resultChan <-chan FrameResult, stop *atomic.Bool, wg *sync.WaitGroup) {
	defer wg.Done()

	var buffer []FrameResult
	ticker := time.NewTicker(500 * time.Millisecond) // Enviar a cada 500ms
	defer ticker.Stop()

	flush := func() {
		if len(buffer) == 0 {
			return
		}

		data, err := EncodeGob(&buffer)
		if err != nil {
			log.Printf("⚠️ Encode result error: %v", err)
			buffer = buffer[:0]
			return
		}

		// Lógica de retentativa
		for i := 0; i < 5; i++ {
			if _, err := w.httpPost("/batch", data); err == nil {
				break
			} else {
				log.Printf("⚠️ Send error (retry %d): %v", i+1, err)
				time.Sleep(500 * time.Millisecond)
			}
		}

		buffer = buffer[:0] // Limpar buffer (manter capacidade)
	}

	for {
		select {
		case result, ok := <-resultChan:
			if !ok {
				flush() // Enviar restantes
				return
			}
			buffer = append(buffer, result)
			if len(buffer) >= BatchSize {
				flush()
			}
		case <-ticker.C:
			flush()
		}
	}
}

// monitorLoop: Exibe stats de FPS
func (w *Worker) monitorLoop(stop *atomic.Bool) {
	start := time.Now()
	lastCount := int64(0)
	lastTime := start

	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for !stop.Load() {
		<-ticker.C
		now := time.Now()
		total := w.processed.Load()

		diff := total - lastCount
		elapsed := now.Sub(lastTime).Seconds()
		fps := float64(diff) / elapsed

		if diff > 0 {
			fmt.Printf("\r🚀 Worker FPS: %.1f | Total: %d   ", fps, total)
		}

		lastCount = total
		lastTime = now
	}
}

// Lógica processFrame
func (w *Worker) processFrame(job FrameJob, ecc *encoder.ECCEncoder, img *image.RGBA) FrameResult {
	// 1. Criar Frame (ECC + Dados)
	frame, err := encoder.NewFrame(
		w.frameCfg, ecc, job.FrameIndex, job.Data,
		w.config.TotalFrames, w.config.OriginalSize, w.config.FileHash,
	)
	if err != nil {
		return FrameResult{FrameIndex: job.FrameIndex, Error: err.Error()}
	}

	// 2. Renderizar pixels
	// Nota: Usa buffer 'img' compartilhado...
	pixels, err := frame.Render(nil)
	if err != nil {
		return FrameResult{FrameIndex: job.FrameIndex, Error: err.Error()}
	}

	// 3. Desenhar na Imagem
	// Desenhar fundo se necessário.
	// For simplicity and speed:
	w.renderCalibrationBar(img) // Partes estáticas

	// Partes dinâmicas
	for _, mp := range pixels {
		offsetY := mp.Y + w.frameCfg.CalibrationHeight
		gray := mp.ByteToGray()

		// Otimização de loop para velocidade
		baseOffset := offsetY*img.Stride + mp.X*4

		// Assumindo mp.Size pequeno
		for y := 0; y < mp.Size; y++ {
			rowOffset := baseOffset + y*img.Stride
			for x := 0; x < mp.Size; x++ {
				off := rowOffset + x*4
				if off+3 < len(img.Pix) {
					img.Pix[off] = gray
					img.Pix[off+1] = gray
					img.Pix[off+2] = gray
					img.Pix[off+3] = 255
				}
			}
		}
	}

	// 4. Comprimir
	compressed := CompressPixels(img.Pix)

	return FrameResult{
		FrameIndex:       job.FrameIndex,
		CompressedPixels: compressed,
		Width:            w.frameCfg.Width,
		Height:           w.frameCfg.Height,
	}
}

func (w *Worker) renderCalibrationBar(img *image.RGBA) {
	// Otimização: Poderia ser pré-renderizado
	width := img.Bounds().Dx()
	sectionWidth := width / 4
	calHeight := w.frameCfg.CalibrationHeight

	// Fill white/black/white/black pattern
	// Preenchimento rápido
	for y := 0; y < calHeight; y++ {
		rowOffset := y * img.Stride
		for x := 0; x < width; x++ {
			var val uint8 = 0
			if (x >= sectionWidth && x < sectionWidth*2) || x >= sectionWidth*3 {
				val = 255
			}
			off := rowOffset + x*4
			img.Pix[off] = val
			img.Pix[off+1] = val
			img.Pix[off+2] = val
			img.Pix[off+3] = 255
		}
	}
}

func (w *Worker) httpGet(path string) ([]byte, error) {
	resp, err := w.client.Get(w.MasterURL + path)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

func (w *Worker) httpPost(path string, body []byte) ([]byte, error) {
	resp, err := w.client.Post(w.MasterURL+path, "application/octet-stream", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}
