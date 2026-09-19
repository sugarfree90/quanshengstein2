package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

// =====================================================================
// --- RAM-BASED LOGGING SYSTEM & ROTATOR (/dev/shm) ---
// =====================================================================

// RAMLogWriter implements a rotating log file writer in memory (RAM /dev/shm).
// It prevents flash wear on MicroSD cards and eMMC on SBCs like Orange Pi / Raspberry Pi.
type RAMLogWriter struct {
	mu          sync.Mutex
	filePath    string
	maxBytes    int64
	maxBackups  int
	file        *os.File
	currentSize int64
}

// NewRAMLogWriter creates or opens a rotating log writer.
func NewRAMLogWriter(filePath string, maxBytes int64, maxBackups int) (*RAMLogWriter, error) {
	// If path uses /dev/shm but /dev/shm does not exist (e.g. on Windows or non-standard Linux),
	// fall back to the system temp directory so the application runs seamlessly anywhere.
	if strings.HasPrefix(filePath, "/dev/shm") {
		if _, err := os.Stat("/dev/shm"); os.IsNotExist(err) {
			fallbackDir := filepath.Join(os.TempDir(), "catwebservice_shm")
			_ = os.MkdirAll(fallbackDir, 0755)
			filePath = filepath.Join(fallbackDir, filepath.Base(filePath))
			log.Printf("[Logger] Notice: /dev/shm not found (running on %s). Using fallback RAM path: %s", runtime.GOOS, filePath)
		}
	}

	dir := filepath.Dir(filePath)
	if dir != "" && dir != "." {
		_ = os.MkdirAll(dir, 0755)
	}

	w := &RAMLogWriter{
		filePath:   filePath,
		maxBytes:   maxBytes,
		maxBackups: maxBackups,
	}

	if err := w.openFile(); err != nil {
		return nil, err
	}

	return w, nil
}

func (w *RAMLogWriter) openFile() error {
	fi, err := os.Stat(w.filePath)
	if err == nil {
		w.currentSize = fi.Size()
	} else {
		w.currentSize = 0
	}

	f, err := os.OpenFile(w.filePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return err
	}
	w.file = f
	return nil
}

func (w *RAMLogWriter) rotate() error {
	if w.file != nil {
		_ = w.file.Close()
		w.file = nil
	}

	for i := w.maxBackups; i >= 1; i-- {
		src := fmt.Sprintf("%s.%d", w.filePath, i-1)
		if i == 1 {
			src = w.filePath
		}
		dst := fmt.Sprintf("%s.%d", w.filePath, i)

		if _, err := os.Stat(src); err == nil {
			_ = os.Rename(src, dst)
		}
	}

	return w.openFile()
}

func (w *RAMLogWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	writeLen := int64(len(p))
	if w.maxBytes > 0 && (w.currentSize+writeLen) > w.maxBytes {
		if err := w.rotate(); err != nil {
			// Fallback: write to existing file if rotation fails
			if w.file == nil {
				_ = w.openFile()
			}
		}
	}

	if w.file == nil {
		if err := w.openFile(); err != nil {
			return 0, err
		}
	}

	n, err := w.file.Write(p)
	w.currentSize += int64(n)
	return n, err
}

func (w *RAMLogWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file != nil {
		err := w.file.Close()
		w.file = nil
		return err
	}
	return nil
}

// =====================================================================
// --- IN-MEMORY RING BUFFER (FOR WEB CLIENTS & LIVE STREAMING) ---
// =====================================================================

type LogRingBuffer struct {
	mu          sync.RWMutex
	capacity    int
	lines       []string
	subscribers map[chan []byte]bool
}

var globalLogBuffer = &LogRingBuffer{
	capacity:    500,
	lines:       make([]string, 0, 500),
	subscribers: make(map[chan []byte]bool),
}

func (b *LogRingBuffer) Write(p []byte) (int, error) {
	text := string(p)
	rawLines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")

	b.mu.Lock()
	var newLines []string
	for _, l := range rawLines {
		trimmed := strings.TrimRight(l, " \t\r")
		if trimmed != "" {
			b.lines = append(b.lines, trimmed)
			if len(b.lines) > b.capacity {
				b.lines = b.lines[len(b.lines)-b.capacity:]
			}
			newLines = append(newLines, trimmed)
		}
	}

	// Dispatch to active web subscribers via non-blocking channel send
	if len(b.subscribers) > 0 && len(newLines) > 0 {
		for _, nl := range newLines {
			msg, err := json.Marshal(map[string]interface{}{
				"cmd":  "log_entry",
				"line": nl,
			})
			if err != nil {
				continue
			}
			for ch := range b.subscribers {
				select {
				case ch <- msg:
				default:
					// Drop if channel buffer is temporarily full to avoid blocking
				}
			}
		}
	}
	b.mu.Unlock()

	return len(p), nil
}

func (b *LogRingBuffer) GetLines() []string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	result := make([]string, len(b.lines))
	copy(result, b.lines)
	return result
}

func (b *LogRingBuffer) Subscribe(ch chan []byte) {
	if ch == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.subscribers[ch] = true
}

func (b *LogRingBuffer) Unsubscribe(ch chan []byte) {
	if ch == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.subscribers, ch)
}

// =====================================================================
// --- INITIALIZATION & MULTI-WRITER SETUP ---
// =====================================================================

var activeLogWriter io.WriteCloser

// initLogger configures Go's standard logging subsystem according to appCfg.
func initLogger(cfg Config) {
	var writers []io.Writer

	mode := strings.ToLower(strings.TrimSpace(cfg.LogMode))
	if mode == "" {
		mode = "shm"
	}

	if mode != "off" && mode != "none" {
		maxSize := int64(cfg.LogMaxSizeMB) * 1024 * 1024
		if maxSize <= 0 {
			maxSize = 2 * 1024 * 1024 // Default: 2 MB
		}
		maxBackups := cfg.LogMaxBackups
		if maxBackups <= 0 {
			maxBackups = 1 // Default: 1 backup (.1)
		}

		filePath := cfg.LogPath
		if filePath == "" {
			filePath = "/dev/shm/catwebservice.log"
		}

		ramWriter, err := NewRAMLogWriter(filePath, maxSize, maxBackups)
		if err != nil {
			log.Printf("[Logger] Failed to create RAM log writer at %s: %v", filePath, err)
		} else {
			activeLogWriter = ramWriter
			writers = append(writers, ramWriter)
			log.Printf("[Logger] MicroSD wear protection active: Logging to RAM (%s, max %d MB, %d backup)", filePath, cfg.LogMaxSizeMB, maxBackups)
		}
	}

	// Always keep in-memory ring buffer active for browser live console
	writers = append(writers, globalLogBuffer)

	// Optionally mirror to stdout for systemd (journalctl -u catwebservice -f)
	if cfg.LogToStdout || len(writers) == 0 {
		writers = append(writers, os.Stdout)
	}

	multi := io.MultiWriter(writers...)
	log.SetOutput(multi)
	log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds)
}

// handleWebLogs handles GET /logs to view recent logs in browser as plain text or SSE.
func handleWebLogs(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	lines := globalLogBuffer.GetLines()
	var buf bytes.Buffer
	buf.WriteString("=== Quansheng Web Transceiver In-Memory RAM Live Logs ===\n\n")
	for _, l := range lines {
		buf.WriteString(l)
		buf.WriteString("\n")
	}
	_, _ = w.Write(buf.Bytes())
}

