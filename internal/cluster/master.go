package cluster

import (
	"fmt"
	"image"
	"io"
	"log"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"ncc/internal/encoder"
)

const BatchSize = 200 // frames por requisição HTTP

// Master: Servidor HTTP que distribui jobs
type Master struct {
	Port     int
	Config   JobConfig
	FrameCfg encoder.FrameConfig
	ECCCfg   encoder.ECCConfig

	// Canal de resultados - lido pelo main.go
	Results chan FrameResult

	// Fila de trabalhos - thread-safe
	jobsMu   sync.Mutex
	jobs     []FrameJob
	jobsDone bool

	// Estatísticas
	JobsSent      atomic.Int64
	JobsCompleted atomic.Int64
	ActiveWorkers atomic.Int64

	// Controle
	running atomic.Bool
}

// NewMaster cria novo servidor master
func NewMaster(port int, frameCfg encoder.FrameConfig, eccCfg encoder.ECCConfig,
	totalFrames int, originalSize uint64, fileHash [32]byte) *Master {

	return &Master{
		Port:     port,
		FrameCfg: frameCfg,
		ECCCfg:   eccCfg,
		Config: JobConfig{
			Width:             frameCfg.Width,
			Height:            frameCfg.Height,
			MacroSize:         frameCfg.MacroSize,
			FPS:               frameCfg.FPS,
			CalibrationHeight: frameCfg.CalibrationHeight,
			GrayLevels:        frameCfg.GrayLevels,
			DataShards:        eccCfg.DataShards,
			ParityShards:      eccCfg.ParityShards,
			TotalFrames:       totalFrames,
			OriginalSize:      originalSize,
			FileHash:          fileHash,
		},
		Results: make(chan FrameResult, 200),
		jobs:    make([]FrameJob, 0, totalFrames),
	}
}

// StartDistribution: Habilita envio de jobs
func (m *Master) StartDistribution() {
	m.running.Store(true)
}

// AddJob adiciona job à fila
func (m *Master) AddJob(job FrameJob) {
	m.jobsMu.Lock()
	defer m.jobsMu.Unlock()
	m.jobs = append(m.jobs, job)
}

// FinishAddingJobs marca fim da adição
func (m *Master) FinishAddingJobs() {
	m.jobsMu.Lock()
	defer m.jobsMu.Unlock()
	m.jobsDone = true
}

// takeBatch remove e retorna até N jobs
func (m *Master) takeBatch(n int) []FrameJob {
	m.jobsMu.Lock()
	defer m.jobsMu.Unlock()
	if len(m.jobs) == 0 {
		return nil
	}
	if n > len(m.jobs) {
		n = len(m.jobs)
	}
	batch := make([]FrameJob, n)
	copy(batch, m.jobs[:n])
	m.jobs = m.jobs[n:]
	return batch
}

// hasJobs retorna se há jobs pendentes
func (m *Master) hasJobs() bool {
	m.jobsMu.Lock()
	defer m.jobsMu.Unlock()
	return len(m.jobs) > 0 || !m.jobsDone
}

// Start: Inicia servidor HTTP (bloqueante)
func (m *Master) Start() error {
	mux := http.NewServeMux()
	mux.HandleFunc("/config", m.handleConfig)
	mux.HandleFunc("/batch", m.handleBatch) // GET: buscar batch, POST: enviar resultados
	mux.HandleFunc("/register", m.handleRegister)
	mux.HandleFunc("/status", m.handleStatus)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "noiseCryptCloud Master - %d active workers\n", m.ActiveWorkers.Load())
	})

	server := &http.Server{
		Addr:         fmt.Sprintf(":%d", m.Port),
		Handler:      mux,
		ReadTimeout:  5 * time.Minute,
		WriteTimeout: 5 * time.Minute,
	}

	fmt.Printf("🖥️  Master listening on :%d\n", m.Port)
	fmt.Println("   Waiting for workers to connect...")
	fmt.Println("   Expose with: cloudflared tunnel --url http://localhost:" + fmt.Sprint(m.Port))

	return server.ListenAndServe()
}

// StartAsync inicia servidor em background
func (m *Master) StartAsync() {
	go func() {
		if err := m.Start(); err != nil && err != http.ErrServerClosed {
			log.Printf("Master server error: %v", err)
		}
	}()
}

// handleRegister: worker se anuncia
func (m *Master) handleRegister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read body error", http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	var info WorkerInfo
	if err := DecodeJSON(body, &info); err != nil {
		http.Error(w, "invalid worker info", http.StatusBadRequest)
		return
	}

	id := m.ActiveWorkers.Add(1)
	fmt.Printf("✅ Worker #%d registered: %s (%s/%s, %d cores)\n",
		id, info.Hostname, info.OS, info.Arch, info.CPUCores)

	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok"))
}

// handleConfig: busca config (GET -> JSON)
func (m *Master) handleConfig(w http.ResponseWriter, r *http.Request) {
	data, err := EncodeJSON(m.Config)
	if err != nil {
		http.Error(w, "encode config error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(data)
}

// handleBatch: GET (busca) e POST (envia)
func (m *Master) handleBatch(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		m.handleGetBatch(w, r)
	case http.MethodPost:
		m.handlePostBatch(w, r)
	default:
		http.Error(w, "GET or POST only", http.StatusMethodNotAllowed)
	}
}

// handleGetBatch: busca lote de jobs
func (m *Master) handleGetBatch(w http.ResponseWriter, r *http.Request) {
	if !m.running.Load() {
		w.WriteHeader(http.StatusAccepted) // 202 = Aguarde, servidor não iniciado
		return
	}

	batch := m.takeBatch(BatchSize)
	if batch == nil {
		if !m.hasJobs() {
			w.WriteHeader(http.StatusNoContent) // 204 = tudo pronto
			return
		}
		w.WriteHeader(http.StatusAccepted) // 202 = tente novamente
		return
	}

	data, err := EncodeGob(&batch)
	if err != nil {
		// Devolver batch
		m.jobsMu.Lock()
		m.jobs = append(batch, m.jobs...)
		m.jobsMu.Unlock()
		http.Error(w, "encode error", http.StatusInternalServerError)
		return
	}

	m.JobsSent.Add(int64(len(batch)))
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Write(data)
}

// handlePostBatch: Worker envia resultados
func (m *Master) handlePostBatch(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read body error", http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	var results []FrameResult
	if err := DecodeGob(body, &results); err != nil {
		http.Error(w, "decode error: "+err.Error(), http.StatusBadRequest)
		return
	}

	for _, result := range results {
		m.JobsCompleted.Add(1)
		m.Results <- result
	}

	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, "ok:%d", len(results))
}

// handleStatus: info de progresso
func (m *Master) handleStatus(w http.ResponseWriter, r *http.Request) {
	m.jobsMu.Lock()
	pending := len(m.jobs)
	m.jobsMu.Unlock()

	status := fmt.Sprintf(`{"sent":%d,"completed":%d,"pending":%d,"workers":%d,"total":%d}`,
		m.JobsSent.Load(), m.JobsCompleted.Load(), pending,
		m.ActiveWorkers.Load(), m.Config.TotalFrames)

	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(status))
}

// DecompressResult: Descomprime dados para imagem RGBA
func DecompressResult(result FrameResult, width, height int) (*image.RGBA, error) {
	pixelData, err := DecompressPixels(result.CompressedPixels)
	if err != nil {
		return nil, fmt.Errorf("decompress frame %d: %w", result.FrameIndex, err)
	}

	img := image.NewRGBA(image.Rect(0, 0, width, height))
	expectedLen := width * height * 4
	if len(pixelData) < expectedLen {
		return nil, fmt.Errorf("pixel data too small: got %d, need %d", len(pixelData), expectedLen)
	}
	copy(img.Pix, pixelData[:expectedLen])

	return img, nil
}
