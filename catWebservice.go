package main

import (
	"bufio"
	"bytes"
	crand "crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log"
	"math"
	"math/big"
	"math/rand"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v3"
	"github.com/pion/webrtc/v3/pkg/media"
	"go.bug.st/serial"
)

// =====================================================================
// --- STRUKTURY DANYCH I KONFIGURACJA ---
// =====================================================================

type DTMFReport struct {
	Timestamp string `json:"timestamp"`
	Code      string `json:"code"`
	Dbm       int    `json:"dbm"`
	Freq      int    `json:"freq"`
}

type Config struct {
	Callsign                     string `json:"callsign"`
	SerialPort                   string `json:"serial_port"`
	BaudRate                     int    `json:"baud_rate"`
	WsHost                       string `json:"ws_host"`
	WsPort                       int    `json:"ws_port"`
	HttpsPort                    int    `json:"https_port"`
	CertFile                     string `json:"cert_file"`
	KeyFile                      string `json:"key_file"`
	UseDirewolf                  bool   `json:"use_direwolf"`
	AprsFreq                     int    `json:"aprs_freq"`
	DtmfFreq                     int    `json:"dtmf_freq"`
	BackgroundServicesScanTicks  int     `json:"background_services_scan_ticks"`
	BackgroundServicesScanDelay  float64 `json:"background_services_scan_delay"`
	DirewolfCmd                  string  `json:"direwolf_cmd"`
	AudioRxDevice                string `json:"audio_rx_device"` // e.g. "1,0" or "plughw:1,0" (radio RX device)
	AudioTxDevice                string `json:"audio_tx_device"` // e.g. "1,0" or "plughw:1,0" (radio TX device)
	AudioInputDevice             string `json:"audio_input_device,omitempty"`
	AudioOutputDevice            string `json:"audio_output_device,omitempty"`
	RxAudioCmd                   string `json:"rx_audio_cmd"`
	TxAudioCmd                   string `json:"tx_audio_cmd"`
	AudioRTPPort                 int    `json:"audio_rtp_port"`
	TotLimitSeconds              int    `json:"tot_limit_seconds"`
	MQTTAPREnabled               bool   `json:"mqtt_aprs_enabled"`
	MQTTEnabled                  bool   `json:"mqtt_enabled,omitempty"` // backward compatibility
	MQTTDTMFEnabled              bool   `json:"mqtt_dtmf_enabled"`
	MQTTDTMFTopic                string `json:"mqtt_dtmf_topic"`
	MQTTStatusEnabled            bool   `json:"mqtt_status_enabled"`
	MQTTStatusTopic              string `json:"mqtt_status_topic"`
	MQTTBroker                   string `json:"mqtt_broker"`
	MQTTAPRSTopic                string `json:"mqtt_aprs_topic"`
	MQTTTopicPrefix              string `json:"mqtt_topic_prefix,omitempty"` // backward compatibility
	MQTTClientID                 string `json:"mqtt_client_id"`
	MQTTUsername                 string `json:"mqtt_username"`
	MQTTPassword                 string `json:"mqtt_password"`
	MQTTRetain                   bool   `json:"mqtt_retain"`
	MQTTQoS                      int    `json:"mqtt_qos"`
	OwrxProxyEnabled             bool   `json:"owrx_proxy_enabled"`
	OwrxBackendURL               string `json:"owrx_backend_url"`
	OwrxProxyPort                int    `json:"owrx_proxy_port"`
	LogMode                      string `json:"log_mode"`
	LogPath                      string `json:"log_path"`
	LogMaxSizeMB                 int    `json:"log_max_size_mb"`
	LogMaxBackups                int    `json:"log_max_backups"`
	LogToStdout                  bool   `json:"log_to_stdout"`
}

var defaultCfg = Config{
	Callsign:                    "SP3MM",
	SerialPort:                  "/dev/ttyACM0",
	BaudRate:                    38400,
	WsHost:                      "0.0.0.0",
	WsPort:                      8081,
	HttpsPort:                   8443,
	CertFile:                    "cert.pem",
	KeyFile:                     "key.pem",
	UseDirewolf:                 true,
	AprsFreq:                    144800000,
	DtmfFreq:                    145550000,
	BackgroundServicesScanTicks: 7,
	BackgroundServicesScanDelay: 2.0,
	DirewolfCmd:                 "direwolf -c direwolf.conf -t 0",
	AudioRxDevice:               "1,0",
	AudioTxDevice:               "1,0",
	RxAudioCmd:                  "ffmpeg -hide_banner -loglevel error -f alsa -thread_queue_size 1024 -ar 48000 -ac 1 -i {DEVICE} -c:a libopus -b:a 48k -vbr off -application voip -frame_duration 20 -f rtp rtp://127.0.0.1:4000",
	TxAudioCmd:                  "ffmpeg -hide_banner -loglevel error -f ogg -i pipe:0 -f alsa {DEVICE}",
	AudioRTPPort:                4000,
	TotLimitSeconds:             3600,
	MQTTAPREnabled:              true,
	MQTTDTMFEnabled:             true,
	MQTTDTMFTopic:               "dtmf",
	MQTTStatusEnabled:           true,
	MQTTStatusTopic:             "radio/status",
	MQTTBroker:                  "tcp://11.1.1.50:1883",
	MQTTAPRSTopic:               "aprs",
	MQTTTopicPrefix:             "aprs",
	MQTTClientID:                "quansheng_aprs",
	MQTTUsername:                "",
	MQTTPassword:                "",
	MQTTRetain:                  false,
	MQTTQoS:                     0,
	OwrxProxyEnabled:            true,
	OwrxBackendURL:              "http://127.0.0.1:8073",
	OwrxProxyPort:               8074,
	LogMode:                     "shm",
	LogPath:                     "/dev/shm/catwebservice.log",
	LogMaxSizeMB:                2,
	LogMaxBackups:               1,
	LogToStdout:                 true,
}

func normalizeAlsaDev(dev string) string {
	dev = strings.TrimSpace(dev)
	if dev == "" {
		return "plughw:1,0"
	}
	if !strings.Contains(dev, ":") {
		return "plughw:" + dev
	}
	return dev
}

func getRxAudioCmd() string {
	rxDev := normalizeAlsaDev(appCfg.AudioRxDevice)
	if rxDev == "" {
		rxDev = normalizeAlsaDev(appCfg.AudioInputDevice)
	}
	cmd := appCfg.RxAudioCmd
	if cmd != "" {
		if strings.Contains(cmd, "{DEVICE}") {
			return strings.ReplaceAll(cmd, "{DEVICE}", rxDev)
		}
		words := strings.Fields(cmd)
		for i, w := range words {
			if w == "-i" && i+1 < len(words) {
				words[i+1] = rxDev
				return strings.Join(words, " ")
			}
		}
		return cmd
	}
	return fmt.Sprintf("ffmpeg -hide_banner -loglevel error -f alsa -thread_queue_size 1024 -ar 48000 -ac 1 -i %s -c:a libopus -b:a 48k -vbr off -application voip -frame_duration 20 -f rtp rtp://127.0.0.1:%d", rxDev, appCfg.AudioRTPPort)
}

func getTxAudioCmd() string {
	txDev := normalizeAlsaDev(appCfg.AudioTxDevice)
	if txDev == "" {
		txDev = normalizeAlsaDev(appCfg.AudioOutputDevice)
	}
	cmd := appCfg.TxAudioCmd
	if cmd != "" {
		if strings.Contains(cmd, "{DEVICE}") {
			return strings.ReplaceAll(cmd, "{DEVICE}", txDev)
		}
		words := strings.Fields(cmd)
		for i, w := range words {
			if w == "alsa" && i+1 < len(words) {
				words[i+1] = txDev
				return strings.Join(words, " ")
			}
		}
		return cmd
	}
	return fmt.Sprintf("ffmpeg -hide_banner -loglevel error -f ogg -i pipe:0 -f alsa %s", txDev)
}

func getDirewolfCmd() string {
	cmd := strings.TrimSpace(appCfg.DirewolfCmd)
	if cmd == "" {
		cmd = "direwolf -c direwolf.conf -t 0"
	}
	// Zabezpieczenie przed podaniem stdin '-' bez strumienia audio
	if strings.HasSuffix(cmd, " -") || strings.Contains(cmd, " - ") {
		cmd = strings.ReplaceAll(cmd, "-r 48000", "")
		cmd = strings.ReplaceAll(cmd, "-b 16", "")
		cmd = strings.TrimSuffix(strings.TrimSpace(cmd), "-")
		cmd = strings.TrimSpace(cmd)
	}
	if !strings.Contains(cmd, "-t ") {
		cmd += " -t 0"
	}
	return cmd
}

var (
	appCfg     Config
	scriptDir  string
	cfgFile    string
	dbFile     string
	configLock sync.RWMutex
)

type HistoryEntry struct {
	Freq     int     `json:"freq"`
	Mod      string  `json:"mod"`
	Pwr      int     `json:"pwr"`
	Ctcss    float64 `json:"ctcss"`
	Dcs      int     `json:"dcs"`
	ShiftDir int     `json:"shift_dir"`
	ShiftVal float64 `json:"shift_val"`
}

func (h *HistoryEntry) UnmarshalJSON(data []byte) error {
	type rawHistory struct {
		Freq     json.Number `json:"freq"`
		Mod      string      `json:"mod"`
		Pwr      int         `json:"pwr"`
		Ctcss    float64     `json:"ctcss"`
		Dcs      int         `json:"dcs"`
		ShiftDir int         `json:"shift_dir"`
		ShiftVal float64     `json:"shift_val"`
	}
	var raw rawHistory
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	h.Mod = raw.Mod
	h.Pwr = raw.Pwr
	h.Ctcss = raw.Ctcss
	h.Dcs = raw.Dcs
	h.ShiftDir = raw.ShiftDir
	h.ShiftVal = raw.ShiftVal
	if fInt, err := raw.Freq.Int64(); err == nil {
		h.Freq = int(fInt)
	} else if fFloat, err := raw.Freq.Float64(); err == nil {
		h.Freq = int(math.Round(fFloat))
	}
	return nil
}

type PresetItem struct {
	Name     string  `json:"name"`
	Freq     int     `json:"freq"`
	Mod      string  `json:"mod"`
	Pwr      int     `json:"pwr"`
	Ctcss    float64 `json:"ctcss"`
	Dcs      int     `json:"dcs"`
	ShiftDir int     `json:"shift_dir"`
	ShiftVal float64 `json:"shift_val"`
	Scan     bool    `json:"scan"`
}

func (p *PresetItem) UnmarshalJSON(data []byte) error {
	type rawPreset struct {
		Name     string      `json:"name"`
		Freq     json.Number `json:"freq"`
		Mod      string      `json:"mod"`
		Pwr      int         `json:"pwr"`
		Ctcss    float64     `json:"ctcss"`
		Dcs      int         `json:"dcs"`
		ShiftDir int         `json:"shift_dir"`
		ShiftVal float64     `json:"shift_val"`
		Scan     bool        `json:"scan"`
	}
	var raw rawPreset
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	p.Name = raw.Name
	p.Mod = raw.Mod
	p.Pwr = raw.Pwr
	p.Ctcss = raw.Ctcss
	p.Dcs = raw.Dcs
	p.ShiftDir = raw.ShiftDir
	p.ShiftVal = raw.ShiftVal
	p.Scan = raw.Scan

	if fInt, err := raw.Freq.Int64(); err == nil {
		p.Freq = int(fInt)
	} else if fFloat, err := raw.Freq.Float64(); err == nil {
		p.Freq = int(math.Round(fFloat))
	}
	return nil
}

type ScanConfig struct {
	Action string  `json:"action"` // "pause", "stop", "ignore"
	Ticks  int     `json:"ticks"`  // default 25
	Delay  float64 `json:"delay,omitempty"` // delay time after carrier loss (s)
}

type ChannelTab struct {
	Name    string       `json:"name"`
	Presets []PresetItem `json:"presets"`
}

type RadioDB struct {
	History      []HistoryEntry `json:"history"`
	Tabs         []ChannelTab   `json:"tabs"`
	Presets      []PresetItem   `json:"presets"`
	Scanlist     []int          `json:"scanlist"`
	ScanConfig   ScanConfig     `json:"scan_config"`
	PTTAudioSync bool           `json:"ptt_audio_sync"`
}

func (db *RadioDB) syncPresets() {
	var flat []PresetItem
	for _, t := range db.Tabs {
		flat = append(flat, t.Presets...)
	}
	db.Presets = flat
}

var (
	radioDB   RadioDB
	dbLock    sync.RWMutex
	stateLock sync.RWMutex
)

type RadioState struct {
	Freq     int     `json:"freq"`
	Mod      string  `json:"mod"`
	Pwr      int     `json:"pwr"`
	Ctcss    float64 `json:"ctcss"`
	Dcs      int     `json:"dcs"`
	Monitor  int     `json:"monitor"`
	ShiftDir int     `json:"shift_dir"`
	ShiftVal float64 `json:"shift_val"`
}

var radioState RadioState

// =====================================================================
// --- GLOBAL STATE AND MANAGEMENT VARIABLES ---
// =====================================================================

var (
	isTransmitting         bool
	isTxDraining           bool
	txDrainCancel          chan struct{}
	txAudioSamplesReceived int64
	txStartTime            time.Time
	txLock                 sync.Mutex

	// WebSocket clients
	clients     = make(map[*websocket.Conn]chan []byte)
	clientsLock sync.RWMutex
	upgrader    = websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool { return true },
	}

	// WebRTC Pion
	webrtcAPI           *webrtc.API
	audioTrack          *webrtc.TrackLocalStaticSample
	peerConnections     = make(map[string]*webrtc.PeerConnection)
	peerConnectionsLock sync.RWMutex

	// Governor
	currentAudioMode string
	governorLock     sync.Mutex
	direwolfProc     *exec.Cmd
	rxAudioProc      *exec.Cmd

	// Hardware Scanner
	hwScanActive       bool
	hwScanPaused       bool
	hwScanIsBackground bool
	hwScanList         []int
	hwScanTicks        int = 25
	hwScanIdx          int
	hwScanResumeDelay  float64 = 2.0
	hwScanLock         sync.RWMutex

	// Audio transmission pipeline (TX)
	txAudioQueue   = make(chan []byte, 100)
	txAudioProcess *exec.Cmd
	txAudioStdin   io.WriteCloser
	txAudioSeq     uint32
	txAudioSerial  uint32
	txAudioGranule int64

	// Squelch state tracking for MQTT (active only in browser mode)
	squelchLock     sync.RWMutex
	lastSquelchOpen bool
)

// isBrowserMode returns true if at least one web client is connected and the radio is in VOICE mode.
// In APRS or background services mode, squelch events are ignored to prevent MQTT flooding.
func isBrowserMode() bool {
	clientsLock.RLock()
	cCount := len(clients)
	clientsLock.RUnlock()
	if cCount == 0 {
		return false
	}

	governorLock.Lock()
	mode := currentAudioMode
	governorLock.Unlock()

	hwScanLock.RLock()
	isBg := hwScanIsBackground
	hwScanLock.RUnlock()

	return mode == "VOICE" && !isBg
}

func publishRadioStatus() {
	stateLock.RLock()
	st := radioState
	stateLock.RUnlock()

	txLock.Lock()
	tx := isTransmitting
	txLock.Unlock()

	configLock.RLock()
	callsign := appCfg.Callsign
	configLock.RUnlock()

	sqlOpen := false
	if isBrowserMode() {
		squelchLock.RLock()
		sqlOpen = (lastSquelchOpen || st.Monitor == 1) && !tx
		squelchLock.RUnlock()
	}

	mqttManager.PublishRadioStatus(st, tx, sqlOpen, callsign)
}

// =====================================================================
// --- CONFIGURATION AND DATABASE MANAGEMENT ---
// =====================================================================

// stripJSONComments removes single-line (//) and multi-line (/* */) comments from JSON data,
// while preserving string literals containing slashes (such as URLs).
func stripJSONComments(data []byte) []byte {
	out := make([]byte, 0, len(data))
	inString := false
	escape := false
	i := 0
	n := len(data)
	for i < n {
		if inString {
			out = append(out, data[i])
			if escape {
				escape = false
			} else if data[i] == '\\' {
				escape = true
			} else if data[i] == '"' {
				inString = false
			}
			i++
			continue
		}

		if data[i] == '"' {
			inString = true
			out = append(out, data[i])
			i++
			continue
		}

		// Line comment //
		if i+1 < n && data[i] == '/' && data[i+1] == '/' {
			i += 2
			for i < n && data[i] != '\n' {
				i++
			}
			continue
		}

		// Block comment /* */
		if i+1 < n && data[i] == '/' && data[i+1] == '*' {
			i += 2
			for i+1 < n && !(data[i] == '*' && data[i+1] == '/') {
				i++
			}
			i += 2
			continue
		}

		out = append(out, data[i])
		i++
	}
	return out
}

func loadAppConfig() Config {
	cfg := defaultCfg
	if _, err := os.Stat(cfgFile); os.IsNotExist(err) {
		data, _ := json.MarshalIndent(cfg, "", "    ")
		os.WriteFile(cfgFile, data, 0644)
		return cfg
	}

	data, err := os.ReadFile(cfgFile)
	if err != nil {
		return cfg
	}

	cleanData := stripJSONComments(data)

	var raw map[string]interface{}
	if err := json.Unmarshal(cleanData, &raw); err != nil {
		log.Printf("[Config] Warning: Failed to parse %s: %v. Using defaults.", cfgFile, err)
		return cfg
	}

	_ = json.Unmarshal(cleanData, &cfg)

	// Populate new or missing fields with defaults
	modified := false
	if cfg.Callsign == "" {
		cfg.Callsign = defaultCfg.Callsign
		modified = true
	}
	if cfg.AudioRTPPort == 0 {
		cfg.AudioRTPPort = defaultCfg.AudioRTPPort
		modified = true
	}
	if cfg.RxAudioCmd == "" || strings.Contains(cfg.RxAudioCmd, "-application lowdelay") || strings.Contains(cfg.RxAudioCmd, "-vbr on") {
		log.Println("[Config] Ensuring optimized rx_audio_cmd configuration (voip CBR)...")
		cfg.RxAudioCmd = defaultCfg.RxAudioCmd
		modified = true
	}
	if cfg.TxAudioCmd == "" {
		cfg.TxAudioCmd = defaultCfg.TxAudioCmd
		modified = true
	}
	if cfg.AudioRxDevice == "" {
		if cfg.AudioInputDevice != "" {
			cfg.AudioRxDevice = cfg.AudioInputDevice
		} else {
			cfg.AudioRxDevice = defaultCfg.AudioRxDevice
		}
		modified = true
	}
	if cfg.AudioTxDevice == "" {
		if cfg.AudioOutputDevice != "" {
			cfg.AudioTxDevice = cfg.AudioOutputDevice
		} else {
			cfg.AudioTxDevice = defaultCfg.AudioTxDevice
		}
		modified = true
	}
	if cfg.HttpsPort == 0 {
		cfg.HttpsPort = defaultCfg.HttpsPort
		modified = true
	}
	if cfg.CertFile == "" {
		cfg.CertFile = defaultCfg.CertFile
		modified = true
	}
	if cfg.KeyFile == "" {
		cfg.KeyFile = defaultCfg.KeyFile
		modified = true
	}
	if cfg.TotLimitSeconds == 0 {
		cfg.TotLimitSeconds = defaultCfg.TotLimitSeconds
		modified = true
	}
	if cfg.MQTTBroker == "" {
		cfg.MQTTBroker = defaultCfg.MQTTBroker
		modified = true
	}
	if cfg.MQTTAPRSTopic == "" && cfg.MQTTTopicPrefix != "" {
		cfg.MQTTAPRSTopic = cfg.MQTTTopicPrefix
		modified = true
	}
	if cfg.MQTTAPRSTopic == "" {
		cfg.MQTTAPRSTopic = defaultCfg.MQTTAPRSTopic
		modified = true
	}
	cfg.MQTTTopicPrefix = cfg.MQTTAPRSTopic
	if cfg.MQTTClientID == "" {
		cfg.MQTTClientID = defaultCfg.MQTTClientID
		modified = true
	}
	if cfg.MQTTDTMFTopic == "" {
		cfg.MQTTDTMFTopic = defaultCfg.MQTTDTMFTopic
		modified = true
	}
	if cfg.MQTTStatusTopic == "" {
		cfg.MQTTStatusTopic = defaultCfg.MQTTStatusTopic
		modified = true
	}
	if cfg.DtmfFreq == 0 {
		cfg.DtmfFreq = defaultCfg.DtmfFreq
		modified = true
	}
	if cfg.BackgroundServicesScanTicks == 0 {
		cfg.BackgroundServicesScanTicks = defaultCfg.BackgroundServicesScanTicks
		modified = true
	}
	if cfg.BackgroundServicesScanDelay <= 0 {
		cfg.BackgroundServicesScanDelay = defaultCfg.BackgroundServicesScanDelay
		modified = true
	}
	// Backward compatibility with mqtt_enabled
	if !cfg.MQTTAPREnabled && cfg.MQTTEnabled {
		cfg.MQTTAPREnabled = cfg.MQTTEnabled
		modified = true
	}
	if cfg.OwrxBackendURL == "" {
		cfg.OwrxBackendURL = defaultCfg.OwrxBackendURL
		modified = true
	}
	if cfg.OwrxProxyPort == 0 {
		cfg.OwrxProxyPort = defaultCfg.OwrxProxyPort
		modified = true
	}
	if cfg.LogMode == "" {
		cfg.LogMode = defaultCfg.LogMode
		modified = true
	}
	if cfg.LogPath == "" {
		cfg.LogPath = defaultCfg.LogPath
		modified = true
	}
	if cfg.LogMaxSizeMB == 0 {
		cfg.LogMaxSizeMB = defaultCfg.LogMaxSizeMB
		modified = true
	}
	if cfg.LogMaxBackups == 0 {
		cfg.LogMaxBackups = defaultCfg.LogMaxBackups
		modified = true
	}

	if modified && !bytes.Contains(data, []byte("//")) && !bytes.Contains(data, []byte("/*")) {
		data, err := json.MarshalIndent(cfg, "", "    ")
		if err == nil {
			_ = os.WriteFile(cfgFile, data, 0644)
		}
	}

	return cfg
}

func loadDB() RadioDB {
	defaultDB := RadioDB{
		History: make([]HistoryEntry, 0),
		Tabs: []ChannelTab{
			{
				Name:    "Main",
				Presets: make([]PresetItem, 0),
			},
		},
		Presets:  make([]PresetItem, 0),
		Scanlist: make([]int, 0),
		ScanConfig: ScanConfig{
			Action: "pause",
			Ticks:  25,
		},
		PTTAudioSync: true,
	}

	if _, err := os.Stat(dbFile); os.IsNotExist(err) {
		return defaultDB
	}

	data, err := os.ReadFile(dbFile)
	if err != nil {
		return defaultDB
	}

	var db RadioDB
	if err := json.Unmarshal(data, &db); err != nil {
		return defaultDB
	}

	if db.History == nil {
		db.History = make([]HistoryEntry, 0)
	}
	if db.Presets == nil {
		db.Presets = make([]PresetItem, 0)
	}

	// Tab migration / fallback
	if len(db.Tabs) == 0 {
		db.Tabs = []ChannelTab{
			{
				Name:    "Main",
				Presets: db.Presets,
			},
		}
	} else {
		for i := range db.Tabs {
			if db.Tabs[i].Presets == nil {
				db.Tabs[i].Presets = make([]PresetItem, 0)
			}
		}
	}
	db.syncPresets()

	if db.Scanlist == nil {
		db.Scanlist = make([]int, 0)
	}
	if db.ScanConfig.Ticks == 0 {
		db.ScanConfig.Ticks = 25
	}
	if db.ScanConfig.Action == "" {
		db.ScanConfig.Action = "pause"
	}
	if db.ScanConfig.Delay <= 0 {
		db.ScanConfig.Delay = 2.0
	}

	return db
}

func saveDB() {
	dbLock.Lock()
	radioDB.syncPresets()
	data, err := json.MarshalIndent(radioDB, "", "    ")
	dbLock.Unlock()

	if err == nil {
		_ = os.WriteFile(dbFile, data, 0644)
	}
}

// =====================================================================
// --- KLASA STEROWANIA KABLEM CAT (QUANSHENG) ---
// =====================================================================

type QuanshengCAT struct {
	portName string
	baudRate int
	port     serial.Port
	lock     sync.Mutex
	rxBuf    []byte
}

func NewQuanshengCAT(portName string, baudRate int) *QuanshengCAT {
	cat := &QuanshengCAT{
		portName: portName,
		baudRate: baudRate,
	}
	cat.Connect()
	return cat
}

func (c *QuanshengCAT) Connect() bool {
	if c.port != nil {
		c.port.Close()
		c.port = nil
	}
	c.rxBuf = nil
	mode := &serial.Mode{BaudRate: c.baudRate}
	p, err := serial.Open(c.portName, mode)
	if err != nil {
		log.Printf("[CAT] Cannot open port %s: %v", c.portName, err)
		return false
	}
	_ = p.SetReadTimeout(20 * time.Millisecond)
	p.SetRTS(false)
	p.SetDTR(false)
	c.port = p
	log.Printf("[CAT] Successfully connected to radio on port %s.", c.portName)
	return true
}

func (c *QuanshengCAT) extractDTMFReports() {
	for {
		idx := bytes.Index(c.rxBuf, []byte("RD"))
		if idx == -1 {
			break
		}
		semi := bytes.IndexByte(c.rxBuf[idx:], ';')
		if semi == -1 {
			break
		}
		end := idx + semi
		rdMsg := string(c.rxBuf[idx : end+1])
		c.rxBuf = append(c.rxBuf[:idx], c.rxBuf[end+1:]...)
		go handleDTMFReport(rdMsg)
	}
}

func (c *QuanshengCAT) drainPending() {
	if c.port == nil {
		return
	}
	c.extractDTMFReports()
	if len(c.rxBuf) > 512 {
		c.rxBuf = c.rxBuf[len(c.rxBuf)-256:]
	}
}

func (c *QuanshengCAT) readUntil(delim byte, timeout time.Duration) (string, error) {
	if c.port == nil {
		return "", fmt.Errorf("port nieotwarty")
	}

	deadline := time.Now().Add(timeout)
	buf := make([]byte, 64)

	for {
		c.extractDTMFReports()
		idx := bytes.IndexByte(c.rxBuf, delim)
		if idx != -1 {
			res := string(c.rxBuf[:idx])
			c.rxBuf = c.rxBuf[idx+1:]
			return res, nil
		}

		if time.Now().After(deadline) {
			break
		}

		n, err := c.port.Read(buf)
		if err != nil {
			return "", err
		}
		if n > 0 {
			c.rxBuf = append(c.rxBuf, buf[:n]...)
		}
	}
	return "", fmt.Errorf("timeout reading from port")
}

func (c *QuanshengCAT) sendRaw(cmd string) error {
	if c.port == nil {
		if !c.Connect() {
			return fmt.Errorf("no connection")
		}
	}
	payload := []byte(cmd + ";")
	_, err := c.port.Write(payload)
	return err
}

func (c *QuanshengCAT) Send(cmd string, expectReply bool) (string, error) {
	c.lock.Lock()
	defer c.lock.Unlock()

	if c.port == nil {
		if !c.Connect() {
			return "", fmt.Errorf("no connection")
		}
	}

	c.drainPending()

	payload := []byte(cmd + ";")
	_, err := c.port.Write(payload)
	if err != nil {
		c.Connect()
		return "", err
	}

	if expectReply {
		reply, err := c.readUntil(';', 300*time.Millisecond)
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(reply), nil
	}
	return "", nil
}

func (c *QuanshengCAT) ScanFast(freqHz int, ticks int) (int, int, bool, error) {
	c.lock.Lock()
	defer c.lock.Unlock()

	if c.port == nil {
		if !c.Connect() {
			return 0, 0, false, fmt.Errorf("no connection")
		}
	}

	freqStr := fmt.Sprintf("%011d", freqHz)
	cmd := fmt.Sprintf("SCF%s,%d;\n", freqStr, ticks)

	for attempt := 0; attempt < 3; attempt++ {
		c.drainPending()

		_, err := c.port.Write([]byte(cmd))
		if err != nil {
			c.Connect()
			return 0, 0, false, err
		}

		// Allow radio PLL time to retune and measure (ticks * 10ms + 35ms margin)
		waitMs := ticks*10 + 35
		time.Sleep(time.Duration(waitMs) * time.Millisecond)

		resp, err := c.readUntil(';', 350*time.Millisecond)
		if err != nil {
			continue
		}

		resp = strings.TrimSpace(resp)
		if strings.Contains(resp, "SQ,busy") {
			time.Sleep(40 * time.Millisecond)
			continue
		}

		expectedPrefix := "SQ" + freqStr
		matchIdx := strings.Index(resp, expectedPrefix)
		if matchIdx != -1 {
			cleaned := strings.Trim(resp[matchIdx:], ";\r\n")
			parts := strings.Split(cleaned, ",")
			if len(parts) >= 5 {
				var dbm, noise int
				fmt.Sscanf(parts[2], "%d", &dbm)
				fmt.Sscanf(parts[3], "%d", &noise)
				isOpen := (noise < 30 && dbm > -115)
				return dbm, noise, isOpen, nil
			}
		}
	}

	return 0, 0, false, fmt.Errorf("no valid SCF response for %s", freqStr)
}

func (c *QuanshengCAT) SetVFOA(freqHz int) {
	c.Send(fmt.Sprintf("FA%011d", freqHz), false)
}

func (c *QuanshengCAT) SetMod(modVal int) {
	c.Send(fmt.Sprintf("MD%d", modVal), false)
}

func (c *QuanshengCAT) SetPower(pwrLvl int) {
	c.Send(fmt.Sprintf("PC%d", pwrLvl), false)
}

func (c *QuanshengCAT) SetMonitor(stateVal int) {
	c.Send(fmt.Sprintf("MO%d", stateVal), false)
}

func (c *QuanshengCAT) TonesOff() {
	c.Send("OF", false)
}

func (c *QuanshengCAT) SetCTCSS(toneHz float64) {
	c.Send(fmt.Sprintf("CT%04d", int(math.Round(toneHz*10.0))), false)
}

func (c *QuanshengCAT) SetDCS(dcsCode int) {
	c.Send(fmt.Sprintf("DT%03d", dcsCode), false)
}

func (c *QuanshengCAT) GetSMeter() (string, error) {
	c.lock.Lock()
	defer c.lock.Unlock()

	if c.port == nil {
		if !c.Connect() {
			return "", fmt.Errorf("no connection")
		}
	}

	c.drainPending()
	_, err := c.port.Write([]byte("S1;"))
	if err != nil {
		c.Connect()
		return "", err
	}

	reply, err := c.readUntil(';', 200*time.Millisecond)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(reply), nil
}

var dtmfRegex = regexp.MustCompile(`RD([0-9A-D*#]),([+-]?\d+)`)

func handleDTMFReport(raw string) {
	m := dtmfRegex.FindStringSubmatch(raw)
	if len(m) < 3 {
		return
	}
	code := m[1]
	dbm, err := strconv.Atoi(m[2])
	if err != nil {
		return
	}

	stateLock.RLock()
	freq := radioState.Freq
	stateLock.RUnlock()
	if freq == 0 {
		configLock.RLock()
		freq = appCfg.DtmfFreq
		configLock.RUnlock()
	}

	report := DTMFReport{
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Code:      code,
		Dbm:       dbm,
		Freq:      freq,
	}

	log.Printf("[DTMF] Received code: '%s' (%+d dBm) on frequency %d Hz", code, dbm, freq)

	// Publish to MQTT broker
	mqttManager.PublishDTMF(report)

	// Send event to connected WebSocket clients
	wsMsg, err := json.Marshal(map[string]interface{}{
		"cmd":       "dtmf_received",
		"code":      code,
		"dbm":       dbm,
		"freq":      freq,
		"timestamp": report.Timestamp,
	})
	if err == nil {
		dispatchToClients(wsMsg)
	}
}

func (c *QuanshengCAT) TxOn() {
	c.lock.Lock()
	defer c.lock.Unlock()
	if c.port != nil {
		c.port.SetRTS(false)
		c.port.SetDTR(true)
	}
}

func (c *QuanshengCAT) RxOn() {
	c.lock.Lock()
	defer c.lock.Unlock()
	if c.port != nil {
		c.port.SetDTR(false)
		c.port.SetRTS(false)
		c.port.ResetInputBuffer()
		c.port.ResetOutputBuffer()
	}
}

var radio *QuanshengCAT

// =====================================================================
// --- OGG OPUS PACKAGING (FOR TX TRANSMISSION WITHOUT CGO) ---
// =====================================================================

var oggCRCTable [256]uint32

func initOggCRC() {
	poly := uint32(0x04c11db7)
	for i := 0; i < 256; i++ {
		r := uint32(i) << 24
		for j := 0; j < 8; j++ {
			if (r & 0x80000000) != 0 {
				r = (r << 1) ^ poly
			} else {
				r <<= 1
			}
		}
		oggCRCTable[i] = r
	}
}

func oggChecksum(data []byte) uint32 {
	var crc uint32
	for _, b := range data {
		crc = (crc << 8) ^ oggCRCTable[byte(crc>>24)^b]
	}
	return crc
}

func makeOggPage(headerType byte, granulePos int64, serialNo uint32, pageSeq uint32, payload []byte) []byte {
	segCount := (len(payload) + 254) / 255
	if segCount == 0 {
		segCount = 1
	}
	header := make([]byte, 27+segCount)
	copy(header[0:4], []byte("OggS"))
	header[4] = 0
	header[5] = headerType
	binary.LittleEndian.PutUint64(header[6:14], uint64(granulePos))
	binary.LittleEndian.PutUint32(header[14:18], serialNo)
	binary.LittleEndian.PutUint32(header[18:22], pageSeq)
	binary.LittleEndian.PutUint32(header[22:26], 0) // Checksum initially 0
	header[26] = byte(segCount)

	rem := len(payload)
	for i := 0; i < segCount; i++ {
		if rem >= 255 {
			header[27+i] = 255
			rem -= 255
		} else {
			header[27+i] = byte(rem)
			rem = 0
		}
	}

	full := append(header, payload...)
	crc := oggChecksum(full)
	binary.LittleEndian.PutUint32(full[22:26], crc)
	return full
}

func makeOpusHead() []byte {
	head := make([]byte, 19)
	copy(head[0:8], []byte("OpusHead"))
	head[8] = 1
	head[9] = 1 // 1 channel (mono)
	binary.LittleEndian.PutUint16(head[10:12], 312)
	binary.LittleEndian.PutUint32(head[12:16], 48000)
	binary.LittleEndian.PutUint16(head[16:18], 0)
	head[18] = 0
	return head
}

func makeOpusTags() []byte {
	vendor := "catWebservice-Go"
	tags := make([]byte, 8+4+len(vendor)+4)
	copy(tags[0:8], []byte("OpusTags"))
	binary.LittleEndian.PutUint32(tags[8:12], uint32(len(vendor)))
	copy(tags[12:12+len(vendor)], []byte(vendor))
	binary.LittleEndian.PutUint32(tags[12+len(vendor):], 0)
	return tags
}

func startTxAudioProcess() {
	txLock.Lock()
	defer txLock.Unlock()

	if txAudioStdin != nil {
		txAudioStdin.Close()
		txAudioStdin = nil
	}
	if txAudioProcess != nil && txAudioProcess.Process != nil {
		_ = txAudioProcess.Process.Kill()
		_ = txAudioProcess.Wait()
		txAudioProcess = nil
	}

	txCmdStr := getTxAudioCmd()
	cmdParts := strings.Fields(txCmdStr)
	if len(cmdParts) == 0 {
		return
	}
	log.Printf("[TX Audio] Starting ALSA audio transmitter: %s", txCmdStr)

	cmd := exec.Command(cmdParts[0], cmdParts[1:]...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		log.Printf("[TX Audio] StdinPipe error: %v", err)
		return
	}

	if err := cmd.Start(); err != nil {
		log.Printf("[TX Audio] Failed to start TX process: %v", err)
		stdin.Close()
		return
	}

	txAudioProcess = cmd
	txAudioStdin = stdin
	txAudioSerial = rand.Uint32()
	txAudioSeq = 0
	txAudioGranule = 0

	// Write Ogg Opus headers (BOS + Tags)
	p0 := makeOggPage(0x02, 0, txAudioSerial, txAudioSeq, makeOpusHead())
	txAudioSeq++
	p1 := makeOggPage(0x00, 0, txAudioSerial, txAudioSeq, makeOpusTags())
	txAudioSeq++

	stdin.Write(p0)
	stdin.Write(p1)
}

func stopTxAudioProcess() {
	txLock.Lock()
	defer txLock.Unlock()

	if txAudioStdin != nil {
		// Send EOS (End Of Stream) page
		pEOS := makeOggPage(0x04, txAudioGranule, txAudioSerial, txAudioSeq, nil)
		txAudioStdin.Write(pEOS)
		txAudioStdin.Close()
		txAudioStdin = nil
	}

	if txAudioProcess != nil && txAudioProcess.Process != nil {
		_ = txAudioProcess.Process.Kill()
		_ = txAudioProcess.Wait()
		txAudioProcess = nil
	}
}

func txAudioPlayerLoop() {
	for rawPacket := range txAudioQueue {
		txLock.Lock()
		if isTransmitting && txAudioStdin != nil && len(rawPacket) > 0 {
			// Check whether input is raw PCM16 (from mobile.html) or an Opus packet
			// An Opus packet from AudioEncoder is usually < 300 bytes
			if len(rawPacket) > 1000 {
				// Direct PCM (e.g. mobile.html)
				_, _ = txAudioStdin.Write(rawPacket)
			} else {
				// Opus 20ms frame = 960 samples at 48kHz
				txAudioGranule += 960
				page := makeOggPage(0x00, txAudioGranule, txAudioSerial, txAudioSeq, rawPacket)
				txAudioSeq++
				_, _ = txAudioStdin.Write(page)
			}
		}
		txLock.Unlock()
	}
}

// =====================================================================
// --- GOVERNOR (APRS AND VOICE PROCESS MANAGER) ---
// =====================================================================

func governorSwitch(mode string) {
	if mode == "APRS" {
		squelchLock.Lock()
		wasOpen := lastSquelchOpen
		lastSquelchOpen = false
		squelchLock.Unlock()
		if wasOpen {
			go publishRadioStatus()
		}
	}

	governorLock.Lock()
	defer governorLock.Unlock()

	if currentAudioMode == mode {
		return
	}

	log.Printf("\n[Governor] Switching audio system to mode: %s", mode)

	// 1. Stop active processes to release the ALSA sound card
	if direwolfProc != nil && direwolfProc.Process != nil {
		_ = direwolfProc.Process.Kill()
		_ = direwolfProc.Wait()
		direwolfProc = nil
	}
	if rxAudioProc != nil && rxAudioProc.Process != nil {
		_ = rxAudioProc.Process.Kill()
		_ = rxAudioProc.Wait()
		rxAudioProc = nil
	}

	time.Sleep(1500 * time.Millisecond)

	// 2. Launch appropriate process depending on requested mode
	if mode == "APRS" && appCfg.UseDirewolf {
		dwCmdStr := getDirewolfCmd()
		log.Printf("[Governor] Starting Direwolf: %s", dwCmdStr)
		parts := strings.Fields(dwCmdStr)
		if len(parts) > 0 {
			direwolfProc = exec.Command(parts[0], parts[1:]...)
			stdoutPipe, errOut := direwolfProc.StdoutPipe()
			stderrPipe, errErr := direwolfProc.StderrPipe()
			if errOut != nil || errErr != nil {
				log.Printf("[Governor] Direwolf pipe error: %v, %v", errOut, errErr)
			}
			if err := direwolfProc.Start(); err != nil {
				log.Printf("[Governor] Failed to start Direwolf: %v", err)
			} else {
				if stdoutPipe != nil {
					go readDirewolfStream(stdoutPipe, os.Stdout)
				}
				if stderrPipe != nil {
					go readDirewolfStream(stderrPipe, os.Stderr)
				}
				go func(proc *exec.Cmd) {
					err := proc.Wait()
					governorLock.Lock()
					defer governorLock.Unlock()
					if direwolfProc == proc {
						log.Printf("[Governor] Direwolf exited (status: %v)", err)
						direwolfProc = nil
					}
				}(direwolfProc)
			}
		}
	} else if mode == "VOICE" {
		rxCmdStr := getRxAudioCmd()
		log.Printf("[Governor] Starting ALSA->RTP audio receiver: %s", rxCmdStr)
		parts := strings.Fields(rxCmdStr)
		if len(parts) > 0 {
			rxAudioProc = exec.Command(parts[0], parts[1:]...)
			rxAudioProc.Stdout = os.Stdout
			rxAudioProc.Stderr = os.Stderr
			if err := rxAudioProc.Start(); err != nil {
				log.Printf("[Governor] Failed to start RX audio stream: %v", err)
			}
		}
	}
	currentAudioMode = mode
}

func readDirewolfStream(r io.Reader, echo *os.File) {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := scanner.Text()
		if echo != nil {
			fmt.Fprintln(echo, line)
		}
		globalAprsParser.FeedLine(line)
	}
}

func governorWatcher() {
	var zeroClientsSince time.Time
	for {
		clientsLock.RLock()
		clientCount := len(clients)
		clientsLock.RUnlock()

		governorLock.Lock()
		curMode := currentAudioMode
		governorLock.Unlock()

		if clientCount > 0 {
			zeroClientsSince = time.Time{}
			hwScanLock.Lock()
			if hwScanIsBackground {
				hwScanActive = false
				hwScanPaused = false
				hwScanIsBackground = false
				log.Printf("[Governor] Detected web client (%d) -> Stopping background services scan", clientCount)
			}
			hwScanLock.Unlock()

			if curMode != "VOICE" {
				log.Printf("[Governor] Detected active web clients (%d) -> Switching to VOICE mode", clientCount)
				governorSwitch("VOICE")
			}
		} else {
			// No web clients connected
			if appCfg.UseDirewolf {
				if curMode == "VOICE" {
					if zeroClientsSince.IsZero() {
						zeroClientsSince = time.Now()
					} else if time.Since(zeroClientsSince) > 3*time.Second {
						log.Printf("[Governor] No web clients for 3s -> Returning to APRS and background services")
						governorSwitch("APRS")
						applyBackgroundServices()
						zeroClientsSince = time.Time{}
					}
				} else if curMode != "APRS" {
					governorSwitch("APRS")
					applyBackgroundServices()
				} else {
					// In APRS mode, ensure background services are running
					hwScanLock.RLock()
					isBgActive := hwScanActive && hwScanIsBackground
					hwScanLock.RUnlock()

					configLock.RLock()
					aprsActive := appCfg.UseDirewolf && appCfg.AprsFreq > 0
					dtmfActive := appCfg.MQTTDTMFEnabled && appCfg.DtmfFreq > 0
					configLock.RUnlock()

					if aprsActive && dtmfActive && !isBgActive {
						applyBackgroundServices()
					}
				}
			} else {
				// Direwolf disabled, but DTMF background service may be active
				if curMode == "VOICE" {
					if zeroClientsSince.IsZero() {
						zeroClientsSince = time.Now()
					} else if time.Since(zeroClientsSince) > 3*time.Second {
						applyBackgroundServices()
						zeroClientsSince = time.Time{}
					}
				} else {
					hwScanLock.RLock()
					isBgActive := hwScanActive && hwScanIsBackground
					hwScanLock.RUnlock()

					configLock.RLock()
					dtmfActive := appCfg.MQTTDTMFEnabled && appCfg.DtmfFreq > 0
					configLock.RUnlock()

					if dtmfActive && !isBgActive {
						applyBackgroundServices()
					}
				}
			}
		}

		// Supervise RX Audio process (ffmpeg) in VOICE mode
		governorLock.Lock()
		if currentAudioMode == "VOICE" {
			if rxAudioProc == nil || rxAudioProc.Process == nil || (rxAudioProc.ProcessState != nil && rxAudioProc.ProcessState.Exited()) {
				log.Println("[Governor] rxAudioProc is not running. Automatically restarting audio receiver...")
				rxCmdStr := getRxAudioCmd()
				parts := strings.Fields(rxCmdStr)
				if len(parts) > 0 {
					rxAudioProc = exec.Command(parts[0], parts[1:]...)
					rxAudioProc.Stdout = os.Stdout
					rxAudioProc.Stderr = os.Stderr
					if err := rxAudioProc.Start(); err != nil {
						log.Printf("[Governor] Failed to restart rxAudioProc: %v", err)
					}
				}
			}
		}
		governorLock.Unlock()

		time.Sleep(1 * time.Second)
	}
}

func tuneRadioBackground(freq int) {
	if freq <= 0 {
		return
	}
	radio.SetVFOA(freq)
	time.Sleep(50 * time.Millisecond)
	radio.TonesOff()
	time.Sleep(50 * time.Millisecond)
	radio.SetMod(4) // FM
	time.Sleep(50 * time.Millisecond)
	radio.SetMonitor(0)

	stateLock.Lock()
	radioState.Freq = freq
	radioState.Mod = "FM"
	radioState.Pwr = 7
	radioState.Ctcss = 0
	radioState.Dcs = 0
	radioState.Monitor = 0
	stateLock.Unlock()

	go publishRadioStatus()
}

func applyBackgroundServices() {
	clientsLock.RLock()
	clientCount := len(clients)
	clientsLock.RUnlock()

	if clientCount > 0 {
		return
	}

	configLock.RLock()
	aprsActive := appCfg.UseDirewolf && appCfg.AprsFreq > 0
	dtmfActive := appCfg.MQTTDTMFEnabled && appCfg.DtmfFreq > 0
	aprsFreq := appCfg.AprsFreq
	dtmfFreq := appCfg.DtmfFreq
	bgTicks := appCfg.BackgroundServicesScanTicks
	configLock.RUnlock()

	if bgTicks <= 0 {
		bgTicks = 7
	}

	if aprsActive && dtmfActive {
		// Both services active -> Alternating APRS and DTMF scan
		hwScanLock.Lock()
		hwScanList = []int{aprsFreq, dtmfFreq}
		hwScanTicks = bgTicks
		hwScanIdx = 0
		hwScanPaused = false
		hwScanActive = true
		hwScanIsBackground = true
		hwScanLock.Unlock()
		log.Printf("[Governor] Background services (APRS: %d Hz, DTMF: %d Hz) -> Alternating scan 'pause' (ticks=%d)", aprsFreq, dtmfFreq, bgTicks)
	} else if aprsActive {
		hwScanLock.Lock()
		if hwScanIsBackground {
			hwScanActive = false
			hwScanPaused = false
			hwScanIsBackground = false
		}
		hwScanLock.Unlock()
		tuneRadioBackground(aprsFreq)
		log.Printf("[Governor] Background APRS service active -> Tuned to %d Hz", aprsFreq)
	} else if dtmfActive {
		hwScanLock.Lock()
		if hwScanIsBackground {
			hwScanActive = false
			hwScanPaused = false
			hwScanIsBackground = false
		}
		hwScanLock.Unlock()
		tuneRadioBackground(dtmfFreq)
		log.Printf("[Governor] Background DTMF service active -> Tuned to %d Hz", dtmfFreq)
	} else {
		hwScanLock.Lock()
		if hwScanIsBackground {
			hwScanActive = false
			hwScanPaused = false
			hwScanIsBackground = false
		}
		hwScanLock.Unlock()
	}
}

// =====================================================================
// --- WEBRTC (PION) BUILT-IN SERVER & WHEP ---
// =====================================================================

func initWebRTC() {
	mediaEngine := &webrtc.MediaEngine{}
	if err := mediaEngine.RegisterDefaultCodecs(); err != nil {
		log.Fatalf("[WebRTC] MediaEngine RegisterDefaultCodecs error: %v", err)
	}
	webrtcAPI = webrtc.NewAPI(webrtc.WithMediaEngine(mediaEngine))

	var err error
	audioTrack, err = webrtc.NewTrackLocalStaticSample(
		webrtc.RTPCodecCapability{
			MimeType:    webrtc.MimeTypeOpus,
			ClockRate:   48000,
			Channels:    2,
			SDPFmtpLine: "minptime=10;useinbandfec=1",
		},
		"audio",
		"pion",
	)
	if err != nil {
		log.Fatalf("[WebRTC] NewTrackLocalStaticSample error: %v", err)
	}

	// Start listening for incoming RTP packets on UDP port for radio audio (RX)
	go rxAudioListener()
}

func rxAudioListener() {
	addr, err := net.ResolveUDPAddr("udp", fmt.Sprintf("127.0.0.1:%d", appCfg.AudioRTPPort))
	if err != nil {
		log.Printf("[WebRTC RTP] ResolveUDPAddr error: %v", err)
		return
	}

	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		log.Printf("[WebRTC RTP] ListenUDP error: %v", err)
		return
	}
	defer conn.Close()

	_ = conn.SetReadBuffer(256 * 1024)

	log.Printf("[WebRTC RTP] Listening for local audio stream ready on 127.0.0.1:%d", appCfg.AudioRTPPort)

	var (
		lastPacketTime = time.Now()
		packetMu       sync.Mutex
	)

	// Keep-Alive loop that sends silent Opus frames when ffmpeg is quiet (e.g. process startup or closed squelch)
	// Ensures the browser ALWAYS receives a steady WebRTC packet stream and never pauses playback.
	go func() {
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()

		// Standardowa 3-bajtowa ramka ciszy Opus (TOC 0xf8 + 0xff, 0xfe)
		opusSilence := []byte{0xf8, 0xff, 0xfe}

		for range ticker.C {
			if audioTrack == nil {
				continue
			}

			governorLock.Lock()
			isVoice := (currentAudioMode == "VOICE")
			governorLock.Unlock()

			txLock.Lock()
			tx := isTransmitting
			txLock.Unlock()

			if !isVoice || tx {
				continue
			}

			packetMu.Lock()
			gap := time.Since(lastPacketTime)
			packetMu.Unlock()

			if gap > 35*time.Millisecond {
				_ = audioTrack.WriteSample(media.Sample{
					Data:     opusSilence,
					Duration: 20 * time.Millisecond,
				})
			}
		}
	}()

	buf := make([]byte, 2048)
	var rtpPkt rtp.Packet

	for {
		n, _, err := conn.ReadFrom(buf)
		if err != nil {
			time.Sleep(20 * time.Millisecond)
			continue
		}

		if n < 12 {
			continue
		}

		governorLock.Lock()
		isVoice := (currentAudioMode == "VOICE")
		governorLock.Unlock()

		txLock.Lock()
		tx := isTransmitting
		txLock.Unlock()

		if !isVoice || tx || audioTrack == nil {
			continue
		}

		if err := rtpPkt.Unmarshal(buf[:n]); err == nil && len(rtpPkt.Payload) > 0 {
			packetMu.Lock()
			lastPacketTime = time.Now()
			packetMu.Unlock()

			_ = audioTrack.WriteSample(media.Sample{
				Data:     rtpPkt.Payload,
				Duration: 20 * time.Millisecond,
			})
		}
	}
}

func createPeerConnection() (*webrtc.PeerConnection, error) {
	config := webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{
			{URLs: []string{"stun:stun.l.google.com:19302"}},
		},
	}

	pc, err := webrtcAPI.NewPeerConnection(config)
	if err != nil {
		return nil, err
	}

	if audioTrack != nil {
		if _, err := pc.AddTrack(audioTrack); err != nil {
			pc.Close()
			return nil, err
		}
	}

	// Handle bidirectional audio (browser microphone via WebRTC to radio)
	pc.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		log.Printf("[WebRTC] Received remote audio track: %s", track.Codec().MimeType)
		for {
			pkt, _, err := track.ReadRTP()
			if err != nil {
				return
			}
			if isTransmitting && len(pkt.Payload) > 0 {
				select {
				case txAudioQueue <- pkt.Payload:
				default:
				}
			}
		}
	})

	pcID := fmt.Sprintf("%p", pc)
	peerConnectionsLock.Lock()
	peerConnections[pcID] = pc
	peerConnectionsLock.Unlock()

	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		if state == webrtc.PeerConnectionStateFailed || state == webrtc.PeerConnectionStateClosed || state == webrtc.PeerConnectionStateDisconnected {
			peerConnectionsLock.Lock()
			delete(peerConnections, pcID)
			peerConnectionsLock.Unlock()
			pc.Close()
		}
	})

	return pc, nil
}

// Handle WHEP standard (WebRTC HTTP Egress Protocol) - endpoint /webrtc-api/radio/whep
func handleWHEP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS, DELETE")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")

	if r.Method == http.MethodOptions || r.Method == http.MethodDelete {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	if r.Method != http.MethodPost {
		http.Error(w, "Metoda niedozwolona", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil || len(body) == 0 {
		http.Error(w, "Pusty lub niepoprawny SDP offer", http.StatusBadRequest)
		return
	}

	pc, err := createPeerConnection()
	if err != nil {
		http.Error(w, fmt.Sprintf("PeerConnection creation error: %v", err), http.StatusInternalServerError)
		return
	}

	offer := webrtc.SessionDescription{
		Type: webrtc.SDPTypeOffer,
		SDP:  string(body),
	}

	if err := pc.SetRemoteDescription(offer); err != nil {
		http.Error(w, fmt.Sprintf("SetRemoteDescription error: %v", err), http.StatusBadRequest)
		pc.Close()
		return
	}

	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		http.Error(w, fmt.Sprintf("CreateAnswer error: %v", err), http.StatusInternalServerError)
		pc.Close()
		return
	}

	gatherComplete := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(answer); err != nil {
		http.Error(w, fmt.Sprintf("SetLocalDescription error: %v", err), http.StatusInternalServerError)
		pc.Close()
		return
	}

	select {
	case <-gatherComplete:
	case <-time.After(1500 * time.Millisecond):
	}

	w.Header().Set("Content-Type", "application/sdp")
	w.WriteHeader(http.StatusCreated)
	w.Write([]byte(pc.LocalDescription().SDP))
}

// =====================================================================
// --- ZADANIA POBOCZNE (TELEMETRIA, SKANER, S-METER, TOT) ---
// =====================================================================

func dispatchToClients(msg []byte) {
	clientsLock.RLock()
	defer clientsLock.RUnlock()
	for _, ch := range clients {
		select {
		case ch <- msg:
		default:
		}
	}
}

func getScanFrequencies() []int {
	dbLock.RLock()
	defer dbLock.RUnlock()

	var freqs []int
	// 1. Check presets with scan flag == true
	for _, p := range radioDB.Presets {
		if p.Scan && p.Freq > 0 {
			freqs = append(freqs, p.Freq)
		}
	}
	// 2. If none marked, fall back to scanlist
	if len(freqs) == 0 {
		for _, f := range radioDB.Scanlist {
			if f > 0 {
				freqs = append(freqs, f)
			}
		}
	}
	// 3. If still empty, load all presets
	if len(freqs) == 0 {
		for _, p := range radioDB.Presets {
			if p.Freq > 0 {
				freqs = append(freqs, p.Freq)
			}
		}
	}
	return freqs
}

func tuneToPresetIfExists(freq int) {
	dbLock.RLock()
	var matchedPreset *PresetItem
	for i := range radioDB.Presets {
		if radioDB.Presets[i].Freq == freq {
			matchedPreset = &radioDB.Presets[i]
			break
		}
	}
	dbLock.RUnlock()

	radio.lock.Lock()
	defer radio.lock.Unlock()

	stateLock.RLock()
	mon := radioState.Monitor
	stateLock.RUnlock()

	if matchedPreset != nil {
		modID := modToID(matchedPreset.Mod)
		_ = radio.sendRaw(fmt.Sprintf("FA%011d", matchedPreset.Freq))
		time.Sleep(35 * time.Millisecond)
		_ = radio.sendRaw(fmt.Sprintf("MD%d", modID))
		time.Sleep(35 * time.Millisecond)
		if matchedPreset.Ctcss > 0 {
			_ = radio.sendRaw(fmt.Sprintf("CT%04d", int(math.Round(matchedPreset.Ctcss*10.0))))
		} else if matchedPreset.Dcs > 0 {
			_ = radio.sendRaw(fmt.Sprintf("DT%03d", matchedPreset.Dcs))
		} else {
			_ = radio.sendRaw("OF")
		}
		time.Sleep(35 * time.Millisecond)
		if matchedPreset.Pwr > 0 {
			_ = radio.sendRaw(fmt.Sprintf("PC%d", matchedPreset.Pwr))
			time.Sleep(20 * time.Millisecond)
		}
		// Reset/refresh squelch gate to immediately unblock audio path in radio
		_ = radio.sendRaw(fmt.Sprintf("MO%d", mon))

		stateLock.Lock()
		radioState.Freq = matchedPreset.Freq
		radioState.Mod = matchedPreset.Mod
		radioState.Pwr = matchedPreset.Pwr
		radioState.Ctcss = matchedPreset.Ctcss
		radioState.Dcs = matchedPreset.Dcs
		stateLock.Unlock()
	} else {
		_ = radio.sendRaw(fmt.Sprintf("FA%011d", freq))
		time.Sleep(30 * time.Millisecond)
		_ = radio.sendRaw(fmt.Sprintf("MO%d", mon))

		stateLock.Lock()
		radioState.Freq = freq
		stateLock.Unlock()
	}

	go publishRadioStatus()
}

func sMeterPoller() {
	var silenceCount int

	for {
		hwScanLock.Lock()
		active := hwScanActive
		paused := hwScanPaused
		hwScanLock.Unlock()

		txLock.Lock()
		tx := isTransmitting
		txLock.Unlock()

		// While the scanner is actively hopping (not paused), sMeterPoller skips S1 radio queries,
		// because SCF already reads the S-Meter for each channel being checked.
		if tx || (active && !paused) {
			silenceCount = 0
			time.Sleep(100 * time.Millisecond)
			continue
		}

		resp, err := radio.GetSMeter()
		if err == nil && strings.Contains(resp, "S1,") {
			cleaned := strings.ReplaceAll(resp, ";", "")
			parts := strings.Split(strings.TrimSpace(cleaned), ",")
			if len(parts) >= 4 {
				var dbm, sql int
				fmt.Sscanf(parts[2], "%d", &dbm)
				fmt.Sscanf(parts[3], "%d", &sql)

				isOpen := (sql == 1)
				if isBrowserMode() {
					squelchLock.Lock()
					changed := (isOpen != lastSquelchOpen)
					lastSquelchOpen = isOpen
					squelchLock.Unlock()

					if changed {
						go publishRadioStatus()
					}
				} else {
					squelchLock.Lock()
					lastSquelchOpen = false
					squelchLock.Unlock()
				}

				msg, _ := json.Marshal(map[string]interface{}{
					"cmd": "s_meter",
					"dbm": dbm,
					"sql": sql,
				})
				dispatchToClients(msg)

				// Independent resume logic in backend:
				// When the scanner stopped on a signal (PAUSED) and squelch closed (sql == 0)
				// for the configured time, the backend resumes scanning.
				if active && paused {
					if sql == 0 {
						silenceCount++

						hwScanLock.RLock()
						isBg := hwScanIsBackground
						resumeDelay := hwScanResumeDelay
						hwScanLock.RUnlock()

						targetCycles := 20
						if isBg {
							configLock.RLock()
							bgDelay := appCfg.BackgroundServicesScanDelay
							configLock.RUnlock()
							if bgDelay > 0 {
								targetCycles = int(math.Round(bgDelay * 10))
							}
						} else {
							if resumeDelay >= 0 {
								targetCycles = int(math.Round(resumeDelay * 10))
							}
						}
						if targetCycles < 0 {
							targetCycles = 0
						}

						if silenceCount >= targetCycles {
							silenceCount = 0
							hwScanLock.Lock()
							hwScanPaused = false
							hwScanLock.Unlock()
							log.Printf("[Scanner] No carrier for %.1fs -> Automatically resuming scan", float64(targetCycles)/10.0)
							dispatchToClients([]byte(`{"cmd":"scan_resumed"}`))
						}
					} else {
						silenceCount = 0
					}
				}
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func hwScannerTask() {
	for {
		hwScanLock.Lock()
		active := hwScanActive
		paused := hwScanPaused
		count := len(hwScanList)
		idx := hwScanIdx
		ticks := hwScanTicks
		hwScanLock.Unlock()

		txLock.Lock()
		tx := isTransmitting
		txLock.Unlock()

		if !active || paused || tx {
			time.Sleep(50 * time.Millisecond)
			continue
		}

		// If scan list is empty, reload from database (user-initiated scan only)
		if count == 0 {
			hwScanLock.RLock()
			isBg := hwScanIsBackground
			hwScanLock.RUnlock()
			if isBg {
				time.Sleep(200 * time.Millisecond)
				continue
			}
			freqs := getScanFrequencies()
			if len(freqs) == 0 {
				time.Sleep(200 * time.Millisecond)
				continue
			}
			hwScanLock.Lock()
			hwScanList = freqs
			count = len(freqs)
			idx = 0
			hwScanIdx = 0
			hwScanLock.Unlock()
		}

		if idx >= count {
			idx = 0
			hwScanLock.Lock()
			hwScanIdx = 0
			hwScanLock.Unlock()
		}

		freq := hwScanList[idx]

		dbm, _, isOpen, err := radio.ScanFast(freq, ticks)
		if err == nil {
			openInt := 0
			if isOpen {
				openInt = 1
			}

			msg, _ := json.Marshal(map[string]interface{}{
				"cmd":  "scan_result",
				"freq": freq,
				"dbm":  dbm,
				"sql":  openInt,
			})
			dispatchToClients(msg)

			if isOpen {
				dbLock.RLock()
				action := radioDB.ScanConfig.Action
				dbLock.RUnlock()

				hwScanLock.Lock()
				isBg := hwScanIsBackground
				if isBg || action == "pause" {
					hwScanPaused = true
					log.Printf("[Scanner] Detected carrier on %d Hz (%d dBm) -> Scanner paused", freq, dbm)
				} else if action == "stop" {
					hwScanActive = false
					log.Printf("[Scanner] Detected carrier on %d Hz (%d dBm) -> Scanner stopped", freq, dbm)
				}
				hwScanLock.Unlock()

				// Retune radio to this preset channel so audio plays immediately
				tuneToPresetIfExists(freq)
			}
		}

		hwScanLock.Lock()
		if len(hwScanList) > 0 {
			hwScanIdx = (hwScanIdx + 1) % len(hwScanList)
		}
		hwScanLock.Unlock()
	}
}

func sysMonitor() {
	for {
		temp := 0.0
		if data, err := os.ReadFile("/sys/class/thermal/thermal_zone0/temp"); err == nil {
			var raw int
			if _, err := fmt.Sscanf(strings.TrimSpace(string(data)), "%d", &raw); err == nil {
				temp = float64(raw) / 1000.0
			}
		}

		cpu := 0.0
		if data, err := os.ReadFile("/proc/loadavg"); err == nil {
			var load1 float64
			if _, err := fmt.Sscanf(string(data), "%f", &load1); err == nil {
				cores := float64(runtime.NumCPU())
				if cores < 1 {
					cores = 1
				}
				cpu = math.Min(100.0, (load1/cores)*100.0)
			}
		}

		msg, _ := json.Marshal(map[string]interface{}{
			"cmd":  "sys_stat",
			"cpu":  math.Round(cpu*10) / 10,
			"temp": math.Round(temp*10) / 10,
		})
		dispatchToClients(msg)
		time.Sleep(2 * time.Second)
	}
}

func watchdogTOT() {
	for {
		txLock.Lock()
		tx := isTransmitting
		start := txStartTime
		txLock.Unlock()

		if tx && time.Since(start).Seconds() > float64(appCfg.TotLimitSeconds) {
			log.Printf("[Watchdog] Transmit time limit (TOT) exceeded! Dropping PTT...")
			radio.RxOn()
			stopTxAudioProcess()

			txLock.Lock()
			isTransmitting = false
			txLock.Unlock()

			stateLock.Lock()
			radioState.Monitor = 0
			stateLock.Unlock()

			msg, _ := json.Marshal(map[string]interface{}{"status": "tx_off"})
			dispatchToClients(msg)
			go publishRadioStatus()
		}
		time.Sleep(1 * time.Second)
	}
}

func modToID(mod string) int {
	switch strings.ToUpper(mod) {
	case "FM", "NFM", "WFM":
		return 4
	case "AM":
		return 5
	case "USB", "LSB":
		return 2
	default:
		return 4
	}
}

func finalizeTxStop(data map[string]interface{}) {
	rxFreq := 0
	if f, ok := data["freq"].(float64); ok {
		rxFreq = int(f)
	}

	stateLock.RLock()
	currFreq := radioState.Freq
	prevMon := radioState.Monitor
	stateLock.RUnlock()

	if rxFreq > 0 && rxFreq != currFreq {
		time.Sleep(100 * time.Millisecond)
		radio.SetVFOA(rxFreq)
		time.Sleep(50 * time.Millisecond)

		ctcss := 0.0
		if c, ok := data["ctcss"].(float64); ok {
			ctcss = c
		}
		dcs := 0
		if d, ok := data["dcs"].(float64); ok {
			dcs = int(d)
		}

		if ctcss > 0 {
			radio.SetCTCSS(ctcss)
		} else if dcs > 0 {
			radio.SetDCS(dcs)
		} else {
			radio.TonesOff()
		}
	}

	// Restore monitor (squelch) state
	radio.SetMonitor(prevMon)

	stateLock.Lock()
	if rxFreq > 0 {
		radioState.Freq = rxFreq
	}
	if c, ok := data["ctcss"].(float64); ok {
		radioState.Ctcss = c
	}
	if d, ok := data["dcs"].(float64); ok {
		radioState.Dcs = int(d)
	}
	stateLock.Unlock()
}

// =====================================================================
// --- KONTROLER WEBSOCKET ---
// =====================================================================

func handleWebSocket(w http.ResponseWriter, r *http.Request) {
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("[WS] Connection upgrade error: %v", err)
		return
	}
	defer ws.Close()

	var writeMu sync.Mutex
	safeWrite := func(data []byte) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		return ws.WriteMessage(websocket.TextMessage, data)
	}

	remoteAddr := r.RemoteAddr
	clientCh := make(chan []byte, 256)
	clientsLock.Lock()
	clients[ws] = clientCh
	count := len(clients)
	clientsLock.Unlock()

	log.Printf("[WS] Web client connected: %s (Active sessions: %d)", remoteAddr, count)

	// Immediately activate VOICE mode for web client
	governorLock.Lock()
	if currentAudioMode != "VOICE" {
		governorLock.Unlock()
		go governorSwitch("VOICE")
	} else {
		governorLock.Unlock()
	}

	defer func() {
		globalLogBuffer.Unsubscribe(clientCh)

		clientsLock.Lock()
		delete(clients, ws)
		close(clientCh)
		remaining := len(clients)
		clientsLock.Unlock()

		log.Printf("[WS] Web client disconnected: %s (Remaining sessions: %d)", remoteAddr, remaining)

		txLock.Lock()
		if txDrainCancel != nil {
			close(txDrainCancel)
			txDrainCancel = nil
		}
		isTxDraining = false
		wasTx := isTransmitting
		if isTransmitting {
			radio.RxOn()
			stopTxAudioProcess()
			isTransmitting = false
		}
		txLock.Unlock()
		if wasTx {
			go publishRadioStatus()
		}
	}()

	// Goroutine sending data to client
	go func() {
		for msg := range clientCh {
			if err := safeWrite(msg); err != nil {
				return
			}
		}
	}()

	// Sync database on connection
	configLock.RLock()
	curCfg := appCfg
	configLock.RUnlock()

	dbLock.RLock()
	syncMsg, _ := json.Marshal(map[string]interface{}{
		"cmd":      "sync_db",
		"db":       radioDB,
		"callsign": curCfg.Callsign,
		"mqtt": map[string]interface{}{
			"aprs_enabled":  curCfg.MQTTAPREnabled,
			"dtmf_enabled":  curCfg.MQTTDTMFEnabled,
			"enabled":       curCfg.MQTTAPREnabled,
			"broker":        curCfg.MQTTBroker,
			"aprs_topic":    curCfg.MQTTAPRSTopic,
			"topic_prefix":  curCfg.MQTTAPRSTopic,
			"dtmf_topic":    curCfg.MQTTDTMFTopic,
			"dtmf_freq":     curCfg.DtmfFreq,
			"bg_scan_ticks": curCfg.BackgroundServicesScanTicks,
			"client_id":     curCfg.MQTTClientID,
			"username":      curCfg.MQTTUsername,
			"retain":        curCfg.MQTTRetain,
			"qos":           curCfg.MQTTQoS,
			"connected":     mqttManager.IsConnected(),
		},
		"owrx": map[string]interface{}{
			"enabled": curCfg.OwrxProxyEnabled,
			"port":    curCfg.OwrxProxyPort,
			"url":     curCfg.OwrxBackendURL,
		},
	})
	dbLock.RUnlock()
	_ = safeWrite(syncMsg)

	hwScanLock.Lock()
	scanActive := hwScanActive
	scanPaused := hwScanPaused
	hwScanLock.Unlock()

	statusMsg, _ := json.Marshal(map[string]interface{}{
		"cmd":    "scan_status",
		"active": scanActive,
		"paused": scanPaused,
	})
	_ = safeWrite(statusMsg)

	for {
		msgType, msg, err := ws.ReadMessage()
		if err != nil {
			break
		}

		// Receive binary audio data (Opus or PCM) during TX
		if msgType == websocket.BinaryMessage {
			txLock.Lock()
			tx := isTransmitting
			txLock.Unlock()

			if tx && len(msg) > 0 {
				select {
				case txAudioQueue <- msg:
					if len(msg) > 1000 {
						atomic.AddInt64(&txAudioSamplesReceived, int64(len(msg)/2))
					} else {
						atomic.AddInt64(&txAudioSamplesReceived, 960)
					}
				default:
				}
			}
			continue
		}

		var data map[string]interface{}
		if err := json.Unmarshal(msg, &data); err != nil {
			continue
		}

		cmd, _ := data["cmd"].(string)

		switch cmd {
		case "get_logs":
			lines := globalLogBuffer.GetLines()
			resp, _ := json.Marshal(map[string]interface{}{
				"cmd":   "log_history",
				"lines": lines,
			})
			select {
			case clientCh <- resp:
			default:
			}
			globalLogBuffer.Subscribe(clientCh)

		case "unsubscribe_logs":
			globalLogBuffer.Unsubscribe(clientCh)

		case "set_monitor":
			val := 0
			if v, ok := data["val"].(float64); ok {
				val = int(v)
			}
			radio.SetMonitor(val)
			stateLock.Lock()
			radioState.Monitor = val
			stateLock.Unlock()
			go publishRadioStatus()

		case "start_scan":
			var freqs []int
			if list, ok := data["scanlist"].([]interface{}); ok && len(list) > 0 {
				for _, item := range list {
					if f, ok := item.(float64); ok && int(f) > 0 {
						freqs = append(freqs, int(f))
					}
				}
			}
			if len(freqs) == 0 {
				if tFloat, ok := data["tab"].(float64); ok {
					tIdx := int(tFloat)
					dbLock.RLock()
					if tIdx >= 0 && tIdx < len(radioDB.Tabs) {
						for _, p := range radioDB.Tabs[tIdx].Presets {
							if p.Scan && p.Freq > 0 {
								freqs = append(freqs, p.Freq)
							}
						}
						if len(freqs) == 0 {
							for _, p := range radioDB.Tabs[tIdx].Presets {
								if p.Freq > 0 {
									freqs = append(freqs, p.Freq)
								}
							}
						}
					}
					dbLock.RUnlock()
				}
			}
			if len(freqs) == 0 {
				freqs = getScanFrequencies()
			}

			ticks := 25
			if t, ok := data["ticks"].(float64); ok && int(t) > 0 {
				ticks = int(t)
			} else {
				dbLock.RLock()
				if radioDB.ScanConfig.Ticks > 0 {
					ticks = radioDB.ScanConfig.Ticks
				}
				dbLock.RUnlock()
			}

			dbLock.Lock()
			if radioDB.ScanConfig.Ticks != ticks {
				radioDB.ScanConfig.Ticks = ticks
				saveDB()
			}
			dbLock.Unlock()

			hwScanLock.Lock()
			hwScanList = freqs
			hwScanTicks = ticks
			if d, ok := data["delay"].(float64); ok && d >= 0 {
				hwScanResumeDelay = d
			}
			hwScanIdx = 0
			hwScanPaused = false
			hwScanActive = true
			hwScanIsBackground = false
			hwScanLock.Unlock()

			log.Printf("[Scanner] Started scanning %d channels (ticks=%d)", len(freqs), ticks)

		case "stop_scan":
			hwScanLock.Lock()
			hwScanActive = false
			hwScanPaused = false
			hwScanIsBackground = false
			hwScanLock.Unlock()
			log.Printf("[Scanner] Stopped scanning")

			stateLock.RLock()
			curFreq := radioState.Freq
			mon := radioState.Monitor
			stateLock.RUnlock()

			if curFreq > 0 {
				radio.lock.Lock()
				_ = radio.sendRaw(fmt.Sprintf("FA%011d", curFreq))
				time.Sleep(30 * time.Millisecond)
				_ = radio.sendRaw(fmt.Sprintf("MO%d", mon))
				radio.lock.Unlock()
			}

		case "resume_scan":
			hwScanLock.Lock()
			hwScanPaused = false
			hwScanLock.Unlock()
			log.Printf("[Scanner] Resumed scanning")

		case "set_scan_resume_delay":
			if d, ok := data["delay"].(float64); ok && d >= 0 {
				hwScanLock.Lock()
				hwScanResumeDelay = d
				hwScanLock.Unlock()
				dbLock.Lock()
				radioDB.ScanConfig.Delay = d
				dbLock.Unlock()
				saveDB()
				log.Printf("[Scanner] Set Web scanner resume delay: %.1fs", d)
			}

		case "fast_scan":
			freq := 0
			if f, ok := data["freq"].(float64); ok {
				freq = int(f)
			}
			ticks := 25
			if t, ok := data["ticks"].(float64); ok && int(t) > 0 {
				ticks = int(t)
			}
			if freq > 0 {
				dbm, _, isOpen, err := radio.ScanFast(freq, ticks)
				if err == nil {
					openInt := 0
					if isOpen {
						openInt = 1
					}
					msg, _ := json.Marshal(map[string]interface{}{
						"cmd":  "scan_result",
						"freq": freq,
						"dbm":  dbm,
						"sql":  openInt,
					})
					dispatchToClients(msg)
				}
			}

		case "ping":
			resp := map[string]interface{}{
				"cmd": "pong",
			}
			if t, ok := data["t"]; ok {
				resp["t"] = t
			}
			respBytes, _ := json.Marshal(resp)
			_ = safeWrite(respBytes)

		case "toggle_scanlist":
			if presetMap, ok := data["preset"].(map[string]interface{}); ok {
				freq := 0
				if f, ok := presetMap["freq"].(float64); ok {
					freq = int(f)
				}
				name, _ := presetMap["name"].(string)

				dbLock.Lock()
				for tIdx := range radioDB.Tabs {
					for pIdx := range radioDB.Tabs[tIdx].Presets {
						p := &radioDB.Tabs[tIdx].Presets[pIdx]
						if (freq > 0 && p.Freq == freq) || (name != "" && p.Name == name) {
							if stateVal, ok := data["state"].(bool); ok {
								p.Scan = stateVal
							} else {
								p.Scan = !p.Scan
							}
						}
					}
				}
				dbLock.Unlock()
				saveDB()

				hwScanLock.Lock()
				if hwScanActive {
					hwScanList = getScanFrequencies()
				}
				hwScanLock.Unlock()

				dbLock.RLock()
				res, _ := json.Marshal(map[string]interface{}{
					"cmd":      "sync_db",
					"db":       radioDB,
					"callsign": appCfg.Callsign,
				})
				dbLock.RUnlock()
				dispatchToClients(res)
			}

		case "apply_profile":
			rawFreq, ok := data["freq"].(float64)
			if !ok || rawFreq <= 0 {
				break
			}
			freq := int(rawFreq)
			modStr, _ := data["mod"].(string)
			if modStr == "" {
				modStr = "FM"
			}
			modID := modToID(modStr)

			pwr := 7
			if p, ok := data["pwr"].(float64); ok {
				pwr = int(p)
			}
			ctcss := 0.0
			if c, ok := data["ctcss"].(float64); ok {
				ctcss = c
			}
			dcs := 0
			if d, ok := data["dcs"].(float64); ok {
				dcs = int(d)
			}
			shiftDir := 0
			if sd, ok := data["shift_dir"].(float64); ok {
				shiftDir = int(sd)
			}
			shiftVal := 0.0
			if sv, ok := data["shift_val"].(float64); ok {
				shiftVal = sv
			}

			txLock.Lock()
			txActive := isTransmitting
			txLock.Unlock()
			if txActive {
				break
			}

			radio.lock.Lock()
			stateLock.RLock()
			alreadySet := (radioState.Freq == freq &&
				radioState.Mod == modStr &&
				radioState.Pwr == pwr &&
				math.Abs(radioState.Ctcss-ctcss) < 0.01 &&
				radioState.Dcs == dcs &&
				radioState.ShiftDir == shiftDir &&
				math.Abs(radioState.ShiftVal-shiftVal) < 0.001)
			mon := radioState.Monitor
			stateLock.RUnlock()

			if alreadySet {
				radio.lock.Unlock()
				break
			}

			_ = radio.sendRaw(fmt.Sprintf("FA%011d", freq))
			time.Sleep(35 * time.Millisecond)
			if ctcss > 0 {
				_ = radio.sendRaw(fmt.Sprintf("CT%04d", int(math.Round(ctcss*10.0))))
			} else if dcs > 0 {
				_ = radio.sendRaw(fmt.Sprintf("DT%03d", dcs))
			} else {
				_ = radio.sendRaw("OF")
			}
			time.Sleep(35 * time.Millisecond)
			_ = radio.sendRaw(fmt.Sprintf("MD%d", modID))
			time.Sleep(35 * time.Millisecond)
			_ = radio.sendRaw(fmt.Sprintf("PC%d", pwr))
			time.Sleep(35 * time.Millisecond)
			_ = radio.sendRaw(fmt.Sprintf("MO%d", mon))

			stateLock.Lock()
			radioState.Freq = freq
			radioState.Mod = modStr
			radioState.Pwr = pwr
			radioState.Ctcss = ctcss
			radioState.Dcs = dcs
			radioState.ShiftDir = shiftDir
			radioState.ShiftVal = shiftVal
			stateLock.Unlock()

			radio.lock.Unlock()

			go publishRadioStatus()

		case "update_tabs":
			if rawTabs, ok := data["tabs"].([]interface{}); ok {
				tabsData, _ := json.Marshal(rawTabs)
				var newTabs []ChannelTab
				if json.Unmarshal(tabsData, &newTabs) == nil {
					dbLock.Lock()
					radioDB.Tabs = newTabs
					dbLock.Unlock()
					saveDB()

					hwScanLock.Lock()
					if hwScanActive {
						hwScanList = getScanFrequencies()
					}
					hwScanLock.Unlock()

					dbLock.RLock()
					res, _ := json.Marshal(map[string]interface{}{
						"cmd":      "sync_db",
						"db":       radioDB,
						"callsign": appCfg.Callsign,
					})
					dbLock.RUnlock()
					dispatchToClients(res)
				}
			}

		case "add_tab":
			name, _ := data["name"].(string)
			name = strings.TrimSpace(name)
			if name == "" {
				name = "New Tab"
			}
			dbLock.Lock()
			radioDB.Tabs = append(radioDB.Tabs, ChannelTab{
				Name:    name,
				Presets: make([]PresetItem, 0),
			})
			dbLock.Unlock()
			saveDB()

			dbLock.RLock()
			res, _ := json.Marshal(map[string]interface{}{
				"cmd":      "sync_db",
				"db":       radioDB,
				"callsign": appCfg.Callsign,
			})
			dbLock.RUnlock()
			dispatchToClients(res)

		case "del_tab":
			if tFloat, ok := data["tab"].(float64); ok {
				tIdx := int(tFloat)
				dbLock.Lock()
				if len(radioDB.Tabs) > 1 && tIdx >= 0 && tIdx < len(radioDB.Tabs) {
					radioDB.Tabs = append(radioDB.Tabs[:tIdx], radioDB.Tabs[tIdx+1:]...)
					dbLock.Unlock()
					saveDB()

					hwScanLock.Lock()
					if hwScanActive {
						hwScanList = getScanFrequencies()
					}
					hwScanLock.Unlock()

					dbLock.RLock()
					res, _ := json.Marshal(map[string]interface{}{
						"cmd":      "sync_db",
						"db":       radioDB,
						"callsign": appCfg.Callsign,
					})
					dbLock.RUnlock()
					dispatchToClients(res)
				} else {
					dbLock.Unlock()
				}
			}

		case "move_tab":
			if tFloat, ok := data["tab"].(float64); ok {
				tIdx := int(tFloat)
				dir := 1
				if d, ok := data["direction"].(float64); ok {
					dir = int(d)
				}
				newIdx := tIdx + dir
				dbLock.Lock()
				if tIdx >= 0 && tIdx < len(radioDB.Tabs) && newIdx >= 0 && newIdx < len(radioDB.Tabs) {
					temp := radioDB.Tabs[tIdx]
					radioDB.Tabs[tIdx] = radioDB.Tabs[newIdx]
					radioDB.Tabs[newIdx] = temp
					dbLock.Unlock()
					saveDB()

					dbLock.RLock()
					res, _ := json.Marshal(map[string]interface{}{
						"cmd":      "sync_db",
						"db":       radioDB,
						"callsign": appCfg.Callsign,
					})
					dbLock.RUnlock()
					dispatchToClients(res)
				} else {
					dbLock.Unlock()
				}
			}

		case "rename_tab":
			if tFloat, ok := data["tab"].(float64); ok {
				tIdx := int(tFloat)
				newName, _ := data["name"].(string)
				newName = strings.TrimSpace(newName)
				if newName != "" {
					dbLock.Lock()
					if tIdx >= 0 && tIdx < len(radioDB.Tabs) {
						radioDB.Tabs[tIdx].Name = newName
						dbLock.Unlock()
						saveDB()

						dbLock.RLock()
						res, _ := json.Marshal(map[string]interface{}{
							"cmd":      "sync_db",
							"db":       radioDB,
							"callsign": appCfg.Callsign,
						})
						dbLock.RUnlock()
						dispatchToClients(res)
					} else {
						dbLock.Unlock()
					}
				}
			}

		case "save_preset":
			if presetMap, ok := data["preset"].(map[string]interface{}); ok {
				presetData, _ := json.Marshal(presetMap)
				var item PresetItem
				if json.Unmarshal(presetData, &item) == nil {
					tIdx := 0
					if t, ok := data["tab"].(float64); ok {
						tIdx = int(t)
					}
					dbLock.Lock()
					if len(radioDB.Tabs) == 0 {
						radioDB.Tabs = append(radioDB.Tabs, ChannelTab{Name: "Main", Presets: []PresetItem{}})
					}
					if tIdx >= 0 && tIdx < len(radioDB.Tabs) {
						radioDB.Tabs[tIdx].Presets = append(radioDB.Tabs[tIdx].Presets, item)
					} else {
						radioDB.Tabs[0].Presets = append(radioDB.Tabs[0].Presets, item)
					}
					dbLock.Unlock()
					saveDB()

					dbLock.RLock()
					res, _ := json.Marshal(map[string]interface{}{
						"cmd":      "sync_db",
						"db":       radioDB,
						"callsign": appCfg.Callsign,
					})
					dbLock.RUnlock()
					dispatchToClients(res)
				}
			}

		case "del_preset":
			if idxFloat, ok := data["index"].(float64); ok {
				idx := int(idxFloat)
				tIdx := 0
				if t, ok := data["tab"].(float64); ok {
					tIdx = int(t)
				}
				dbLock.Lock()
				if tIdx >= 0 && tIdx < len(radioDB.Tabs) {
					if idx >= 0 && idx < len(radioDB.Tabs[tIdx].Presets) {
						radioDB.Tabs[tIdx].Presets = append(radioDB.Tabs[tIdx].Presets[:idx], radioDB.Tabs[tIdx].Presets[idx+1:]...)
						dbLock.Unlock()
						saveDB()

						dbLock.RLock()
						res, _ := json.Marshal(map[string]interface{}{
							"cmd":      "sync_db",
							"db":       radioDB,
							"callsign": appCfg.Callsign,
						})
						dbLock.RUnlock()
						dispatchToClients(res)
					} else {
						dbLock.Unlock()
					}
				} else {
					dbLock.Unlock()
				}
			}

		case "update_presets":
			if rawPresets, ok := data["presets"].([]interface{}); ok {
				presetsData, _ := json.Marshal(rawPresets)
				var newPresets []PresetItem
				if json.Unmarshal(presetsData, &newPresets) == nil {
					tIdx := 0
					if t, ok := data["tab"].(float64); ok {
						tIdx = int(t)
					}
					dbLock.Lock()
					if len(radioDB.Tabs) == 0 {
						radioDB.Tabs = append(radioDB.Tabs, ChannelTab{Name: "Main", Presets: []PresetItem{}})
					}
					if tIdx >= 0 && tIdx < len(radioDB.Tabs) {
						radioDB.Tabs[tIdx].Presets = newPresets
					} else {
						radioDB.Tabs[0].Presets = newPresets
					}
					dbLock.Unlock()
					saveDB()

					hwScanLock.Lock()
					if hwScanActive {
						hwScanList = getScanFrequencies()
					}
					hwScanLock.Unlock()

					dbLock.RLock()
					res, _ := json.Marshal(map[string]interface{}{
						"cmd":      "sync_db",
						"db":       radioDB,
						"callsign": appCfg.Callsign,
					})
					dbLock.RUnlock()
					dispatchToClients(res)
				}
			}

		case "save_scan_config":
			if cfgMap, ok := data["config"].(map[string]interface{}); ok {
				cfgData, _ := json.Marshal(cfgMap)
				var newCfg ScanConfig
				if json.Unmarshal(cfgData, &newCfg) == nil {
					dbLock.Lock()
					radioDB.ScanConfig = newCfg
					dbLock.Unlock()
					saveDB()

					hwScanLock.Lock()
					if newCfg.Ticks > 0 {
						hwScanTicks = newCfg.Ticks
					}
					if newCfg.Delay > 0 {
						hwScanResumeDelay = newCfg.Delay
					}
					hwScanLock.Unlock()

					log.Printf("[Scanner] Saved scanner configuration to radio_db.json (ticks=%d, action=%s, delay=%.1fs)", newCfg.Ticks, newCfg.Action, newCfg.Delay)

					dbLock.RLock()
					res, _ := json.Marshal(map[string]interface{}{
						"cmd":      "sync_db",
						"db":       radioDB,
						"callsign": appCfg.Callsign,
					})
					dbLock.RUnlock()
					dispatchToClients(res)
				}
			}

		case "save_ptt_audio_sync":
			if enabled, ok := data["enabled"].(bool); ok {
				dbLock.Lock()
				radioDB.PTTAudioSync = enabled
				dbLock.Unlock()
				saveDB()
				log.Printf("[RadioDB] Saved ptt_audio_sync = %v to radio_db.json", enabled)

				dbLock.RLock()
				res, _ := json.Marshal(map[string]interface{}{
					"cmd":      "sync_db",
					"db":       radioDB,
					"callsign": appCfg.Callsign,
				})
				dbLock.RUnlock()
				dispatchToClients(res)
			}

		case "save_mqtt_config":
			if cfgMap, ok := data["config"].(map[string]interface{}); ok {
				configLock.Lock()
				if v, ok := cfgMap["mqtt_aprs_enabled"].(bool); ok {
					appCfg.MQTTAPREnabled = v
					appCfg.MQTTEnabled = v
				} else if v, ok := cfgMap["mqtt_enabled"].(bool); ok {
					appCfg.MQTTAPREnabled = v
					appCfg.MQTTEnabled = v
				}
				if v, ok := cfgMap["mqtt_dtmf_enabled"].(bool); ok {
					appCfg.MQTTDTMFEnabled = v
				}
				if v, ok := cfgMap["mqtt_status_enabled"].(bool); ok {
					appCfg.MQTTStatusEnabled = v
				}
				if v, ok := cfgMap["mqtt_broker"].(string); ok && strings.TrimSpace(v) != "" {
					appCfg.MQTTBroker = strings.TrimSpace(v)
				}
				if v, ok := cfgMap["mqtt_aprs_topic"].(string); ok && strings.TrimSpace(v) != "" {
					appCfg.MQTTAPRSTopic = strings.TrimSpace(v)
					appCfg.MQTTTopicPrefix = strings.TrimSpace(v)
				} else if v, ok := cfgMap["mqtt_topic_prefix"].(string); ok && strings.TrimSpace(v) != "" {
					appCfg.MQTTAPRSTopic = strings.TrimSpace(v)
					appCfg.MQTTTopicPrefix = strings.TrimSpace(v)
				}
				if v, ok := cfgMap["mqtt_dtmf_topic"].(string); ok && strings.TrimSpace(v) != "" {
					appCfg.MQTTDTMFTopic = strings.TrimSpace(v)
				}
				if v, ok := cfgMap["mqtt_status_topic"].(string); ok && strings.TrimSpace(v) != "" {
					appCfg.MQTTStatusTopic = strings.TrimSpace(v)
				}
				if v, ok := cfgMap["dtmf_freq"].(float64); ok && int(v) > 0 {
					appCfg.DtmfFreq = int(v)
				}
				if v, ok := cfgMap["background_services_scan_ticks"].(float64); ok && int(v) > 0 {
					appCfg.BackgroundServicesScanTicks = int(v)
				}
				if v, ok := cfgMap["mqtt_client_id"].(string); ok {
					appCfg.MQTTClientID = strings.TrimSpace(v)
				}
				if v, ok := cfgMap["mqtt_username"].(string); ok {
					appCfg.MQTTUsername = strings.TrimSpace(v)
				}
				if v, ok := cfgMap["mqtt_password"].(string); ok {
					appCfg.MQTTPassword = v
				}
				if v, ok := cfgMap["mqtt_retain"].(bool); ok {
					appCfg.MQTTRetain = v
				}
				if v, ok := cfgMap["mqtt_qos"].(float64); ok {
					appCfg.MQTTQoS = int(v)
				}
				savedCfg := appCfg
				configLock.Unlock()

				saved, _ := json.MarshalIndent(savedCfg, "", "    ")
				_ = os.WriteFile(cfgFile, saved, 0644)
				log.Printf("[MQTT] Updated MQTT settings in config.json (APRS: %v, DTMF: %v, Status: %v, broker: %s)", savedCfg.MQTTAPREnabled, savedCfg.MQTTDTMFEnabled, savedCfg.MQTTStatusEnabled, savedCfg.MQTTBroker)

				mqttManager.UpdateConfig(savedCfg)

				clientsLock.RLock()
				clientCount := len(clients)
				clientsLock.RUnlock()
				if clientCount == 0 {
					applyBackgroundServices()
				}

				resp, _ := json.Marshal(map[string]interface{}{
					"cmd": "mqtt_config_saved",
					"config": map[string]interface{}{
						"mqtt_aprs_enabled":             savedCfg.MQTTAPREnabled,
						"mqtt_dtmf_enabled":             savedCfg.MQTTDTMFEnabled,
						"mqtt_status_enabled":           savedCfg.MQTTStatusEnabled,
						"mqtt_enabled":                  savedCfg.MQTTAPREnabled,
						"mqtt_broker":                   savedCfg.MQTTBroker,
						"mqtt_aprs_topic":               savedCfg.MQTTAPRSTopic,
						"mqtt_topic_prefix":             savedCfg.MQTTAPRSTopic,
						"mqtt_dtmf_topic":               savedCfg.MQTTDTMFTopic,
						"mqtt_status_topic":             savedCfg.MQTTStatusTopic,
						"dtmf_freq":                     savedCfg.DtmfFreq,
						"background_services_scan_ticks": savedCfg.BackgroundServicesScanTicks,
						"mqtt_client_id":                savedCfg.MQTTClientID,
						"mqtt_username":                 savedCfg.MQTTUsername,
						"mqtt_retain":                   savedCfg.MQTTRetain,
						"mqtt_qos":                      savedCfg.MQTTQoS,
						"connected":                     mqttManager.IsConnected(),
					},
				})
				dispatchToClients(resp)
			}

		case "get_mqtt_config":
			configLock.RLock()
			savedCfg := appCfg
			configLock.RUnlock()

			resp, _ := json.Marshal(map[string]interface{}{
				"cmd": "mqtt_config",
				"config": map[string]interface{}{
					"mqtt_aprs_enabled":             savedCfg.MQTTAPREnabled,
					"mqtt_dtmf_enabled":             savedCfg.MQTTDTMFEnabled,
					"mqtt_status_enabled":           savedCfg.MQTTStatusEnabled,
					"mqtt_enabled":                  savedCfg.MQTTAPREnabled,
					"mqtt_broker":                   savedCfg.MQTTBroker,
					"mqtt_aprs_topic":               savedCfg.MQTTAPRSTopic,
					"mqtt_topic_prefix":             savedCfg.MQTTAPRSTopic,
					"mqtt_dtmf_topic":               savedCfg.MQTTDTMFTopic,
					"mqtt_status_topic":             savedCfg.MQTTStatusTopic,
					"dtmf_freq":                     savedCfg.DtmfFreq,
					"background_services_scan_ticks": savedCfg.BackgroundServicesScanTicks,
					"mqtt_client_id":                savedCfg.MQTTClientID,
					"mqtt_username":                 savedCfg.MQTTUsername,
					"mqtt_retain":                   savedCfg.MQTTRetain,
					"mqtt_qos":                      savedCfg.MQTTQoS,
					"connected":                     mqttManager.IsConnected(),
				},
			})
			_ = safeWrite(resp)

		case "check_audio_system":
			// WebRTC audio readiness: ensure VOICE mode is active
			governorLock.Lock()
			if currentAudioMode != "VOICE" {
				governorLock.Unlock()
				governorSwitch("VOICE")
				governorLock.Lock()
			}
			isReady := (currentAudioMode == "VOICE" && rxAudioProc != nil && rxAudioProc.Process != nil)
			governorLock.Unlock()

			resp, _ := json.Marshal(map[string]interface{}{
				"cmd":   "audio_system_status",
				"ready": isReady,
			})
			_ = safeWrite(resp)

		case "start_tx":
			txLock.Lock()
			if isTxDraining {
				// Seamlessly resume active TX! Cancel drain countdown timer
				if txDrainCancel != nil {
					close(txDrainCancel)
					txDrainCancel = nil
				}
				isTxDraining = false
				isTransmitting = true
				txLock.Unlock()
				log.Printf("[TX Audio] PTT re-pressed during buffer drain: continuing active TX seamlessly")
				_ = safeWrite([]byte(`{"status":"tx_on"}`))
				break
			}
			if isTransmitting {
				txLock.Unlock()
				_ = safeWrite([]byte(`{"status":"tx_on"}`))
				break
			}
			txLock.Unlock()

			atomic.StoreInt64(&txAudioSamplesReceived, 0)

			freq := int(data["freq"].(float64))
			baseFreq := freq
			if bf, ok := data["base_freq"].(float64); ok {
				baseFreq = int(bf)
			}
			modStr, _ := data["mod"].(string)
			if modStr == "" {
				modStr = "FM"
			}
			modID := modToID(modStr)

			pwr := 7
			if p, ok := data["pwr"].(float64); ok {
				pwr = int(p)
			}
			ctcss := 0.0
			if c, ok := data["ctcss"].(float64); ok {
				ctcss = c
			}
			dcs := 0
			if d, ok := data["dcs"].(float64); ok {
				dcs = int(d)
			}
			shiftDir := 0
			if sd, ok := data["shift_dir"].(float64); ok {
				shiftDir = int(sd)
			}
			shiftVal := 0.0
			if sv, ok := data["shift_val"].(float64); ok {
				shiftVal = sv
			}

			// Add to history
			histEntry := HistoryEntry{
				Freq:     baseFreq,
				Mod:      modStr,
				Pwr:      pwr,
				Ctcss:    ctcss,
				Dcs:      dcs,
				ShiftDir: shiftDir,
				ShiftVal: shiftVal,
			}

			dbLock.Lock()
			newHistory := make([]HistoryEntry, 0)
			for _, h := range radioDB.History {
				if h.Freq != histEntry.Freq {
					newHistory = append(newHistory, h)
				}
			}
			radioDB.History = append([]HistoryEntry{histEntry}, newHistory...)
			if len(radioDB.History) > 5 {
				radioDB.History = radioDB.History[:5]
			}
			dbLock.Unlock()
			saveDB()

			dbLock.RLock()
			syncDbMsg, _ := json.Marshal(map[string]interface{}{
				"cmd":      "sync_db",
				"db":       radioDB,
				"callsign": appCfg.Callsign,
			})
			dbLock.RUnlock()
			dispatchToClients(syncDbMsg)

			// Konfiguracja radia pod TX
			radio.SetVFOA(freq)
			time.Sleep(50 * time.Millisecond)
			if ctcss > 0 {
				radio.SetCTCSS(ctcss)
			} else if dcs > 0 {
				radio.SetDCS(dcs)
			} else {
				radio.TonesOff()
			}
			time.Sleep(50 * time.Millisecond)
			radio.SetMod(modID)
			time.Sleep(50 * time.Millisecond)
			radio.SetPower(pwr)
			time.Sleep(150 * time.Millisecond)

			// Clear audio queue
			for len(txAudioQueue) > 0 {
				<-txAudioQueue
			}

			startTxAudioProcess()
			radio.TxOn()

			txLock.Lock()
			isTransmitting = true
			isTxDraining = false
			txStartTime = time.Now()
			txLock.Unlock()

			stateLock.Lock()
			radioState.Freq = freq
			radioState.Pwr = pwr
			radioState.Ctcss = ctcss
			radioState.Dcs = dcs
			radioState.Monitor = 0
			stateLock.Unlock()

			_ = safeWrite([]byte(`{"status":"tx_on"}`))
			go publishRadioStatus()

		case "stop_tx":
			dbLock.RLock()
			syncEnabled := radioDB.PTTAudioSync
			dbLock.RUnlock()

			if pttSyncVal, hasPttSync := data["ptt_sync"].(bool); hasPttSync {
				syncEnabled = pttSyncVal
			}

			txLock.Lock()
			wasTx := isTransmitting
			drainingAlready := isTxDraining
			txLock.Unlock()

			if !wasTx && !drainingAlready {
				break
			}

			if !syncEnabled {
				// Immediate stop (legacy without sync)
				txLock.Lock()
				if txDrainCancel != nil {
					close(txDrainCancel)
					txDrainCancel = nil
				}
				isTxDraining = false
				isTransmitting = false
				txLock.Unlock()

				time.Sleep(150 * time.Millisecond)
				stopTxAudioProcess()
				radio.RxOn()
				finalizeTxStop(data)
				_ = safeWrite([]byte(`{"status":"tx_off"}`))
				go publishRadioStatus()
				break
			}

			// PTT Audio Sync: maintain TX until the audio buffer drains completely
			txLock.Lock()
			if isTxDraining {
				txLock.Unlock()
				break
			}

			totalSamples := atomic.LoadInt64(&txAudioSamplesReceived)
			totalDuration := time.Duration(totalSamples) * time.Second / 48000
			elapsed := time.Since(txStartTime)
			queueDuration := time.Duration(len(txAudioQueue)) * 20 * time.Millisecond

			// Pipeline audio duration waiting to be played (queue + pipe + ALSA buffer)
			remaining := totalDuration - elapsed + 150*time.Millisecond
			if remaining < queueDuration+150*time.Millisecond {
				remaining = queueDuration + 150*time.Millisecond
			}
			if remaining < 150*time.Millisecond {
				remaining = 150 * time.Millisecond
			}

			isTxDraining = true
			cancelCh := make(chan struct{})
			txDrainCancel = cancelCh
			drainMs := int(remaining.Milliseconds())
			txLock.Unlock()

			log.Printf("[TX Audio] PTT released with Audio Sync: draining buffer (%d ms remaining, total samples=%d, elapsed=%v)",
				drainMs, totalSamples, elapsed.Round(time.Millisecond))

			drainStartMsg, _ := json.Marshal(map[string]interface{}{
				"cmd":      "tx_drain_start",
				"drain_ms": drainMs,
			})
			_ = safeWrite(drainStartMsg)

			go func(cancel chan struct{}, totalMs int, stopData map[string]interface{}) {
				ticker := time.NewTicker(50 * time.Millisecond)
				defer ticker.Stop()

				startDrain := time.Now()
				drainDuration := time.Duration(totalMs) * time.Millisecond

				for {
					select {
					case <-cancel:
						log.Printf("[TX Audio] Buffer drain cancelled (new TX started)")
						return
					case now := <-ticker.C:
						elapsedDrain := now.Sub(startDrain)
						rem := drainDuration - elapsedDrain
						remMs := int(rem.Milliseconds())
						if remMs <= 0 {
							txLock.Lock()
							if txDrainCancel != cancel {
								txLock.Unlock()
								return
							}
							txDrainCancel = nil
							isTxDraining = false
							isTransmitting = false
							txLock.Unlock()

							stopTxAudioProcess()
							radio.RxOn()
							finalizeTxStop(stopData)

							log.Printf("[TX Audio] PTT Audio Sync drain completed cleanly. Radio returned to RX.")

							doneMsg, _ := json.Marshal(map[string]interface{}{
								"cmd":    "tx_drain_done",
								"status": "tx_off",
							})
							_ = safeWrite(doneMsg)
							go publishRadioStatus()
							return
						}

						progMsg, _ := json.Marshal(map[string]interface{}{
							"cmd":          "tx_drain_progress",
							"remaining_ms": remMs,
							"total_ms":     totalMs,
						})
						_ = safeWrite(progMsg)
					}
				}
			}(cancelCh, drainMs, data)

		case "heartbeat":
			// Keep-alive (no-op)

		// Handle WebRTC signalling via WebSocket
		case "webrtc_offer":
			if sdp, ok := data["sdp"].(string); ok {
				pc, err := createPeerConnection()
				if err != nil {
					continue
				}
				offer := webrtc.SessionDescription{
					Type: webrtc.SDPTypeOffer,
					SDP:  sdp,
				}
				if err := pc.SetRemoteDescription(offer); err != nil {
					pc.Close()
					continue
				}
				answer, err := pc.CreateAnswer(nil)
				if err != nil {
					pc.Close()
					continue
				}
				gatherComplete := webrtc.GatheringCompletePromise(pc)
				if err := pc.SetLocalDescription(answer); err != nil {
					pc.Close()
					continue
				}
				select {
				case <-gatherComplete:
				case <-time.After(1500 * time.Millisecond):
				}

				resp, _ := json.Marshal(map[string]interface{}{
					"cmd": "webrtc_answer",
					"sdp": pc.LocalDescription().SDP,
				})
				_ = safeWrite(resp)
			}
		}
	}
}

// =====================================================================
// --- HTTP / WEBSOCKET ROUTING / STATIC FILES ---
// =====================================================================

func handleRoot(w http.ResponseWriter, r *http.Request) {
	// Check if this is a WebSocket Upgrade request on the root path
	if websocket.IsWebSocketUpgrade(r) {
		handleWebSocket(w, r)
		return
	}

	path := r.URL.Path
	if path == "/" || path == "/index.html" || path == "/radio.html" {
		target := filepath.Join(scriptDir, "pubhtml", "radio.html")
		if _, err := os.Stat(target); err == nil {
			log.Printf("[HTTP] Serving radio.html for client %s", r.RemoteAddr)
			http.ServeFile(w, r, target)
			return
		}
	}

	if path == "/owrx.js" {
		target := filepath.Join(scriptDir, "pubhtml", "owrx.js")
		if _, err := os.Stat(target); err == nil {
			http.ServeFile(w, r, target)
			return
		}
	}

	// Serve remaining resources from pubhtml or current directory
	pubTarget := filepath.Join(scriptDir, "pubhtml", filepath.Clean(path))
	if _, err := os.Stat(pubTarget); err == nil {
		http.ServeFile(w, r, pubTarget)
		return
	}

	rootTarget := filepath.Join(scriptDir, filepath.Clean(path))
	if _, err := os.Stat(rootTarget); err == nil {
		http.ServeFile(w, r, rootTarget)
		return
	}

	http.NotFound(w, r)
}

// =====================================================================
// --- MAIN LOOP ---
// =====================================================================

func main() {
	initOggCRC()

	// Resolve file paths
	exePath, err := os.Executable()
	if err == nil {
		scriptDir = filepath.Dir(exePath)
	} else {
		scriptDir = "."
	}

	cfgFile = filepath.Join(scriptDir, "config.json")
	dbFile = filepath.Join(scriptDir, "radio_db.json")

	appCfg = loadAppConfig()
	initLogger(appCfg)
	radioDB = loadDB()

	// Check and generate TLS certificates BEFORE initializing any radio hardware, audio, WebRTC, or Direwolf services!
	certPath := appCfg.CertFile
	if certPath == "" {
		certPath = "cert.pem"
	}
	if !filepath.IsAbs(certPath) {
		certPath = filepath.Join(scriptDir, certPath)
	}
	keyPath := appCfg.KeyFile
	if keyPath == "" {
		keyPath = "key.pem"
	}
	if !filepath.IsAbs(keyPath) {
		keyPath = filepath.Join(scriptDir, keyPath)
	}

	ensureTLSCertificates(certPath, keyPath)

	hwScanLock.Lock()
	if radioDB.ScanConfig.Ticks > 0 {
		hwScanTicks = radioDB.ScanConfig.Ticks
	}
	if radioDB.ScanConfig.Delay > 0 {
		hwScanResumeDelay = radioDB.ScanConfig.Delay
	}
	hwScanLock.Unlock()

	// Initialize radio state
	radioState = RadioState{
		Freq:     appCfg.AprsFreq,
		Mod:      "FM",
		Pwr:      7,
		Ctcss:    0,
		Dcs:      0,
		Monitor:  0,
		ShiftDir: 0,
		ShiftVal: 0.0,
	}

	log.Printf("[System] Initializing Quansheng CAT on %s (%d baud)...", appCfg.SerialPort, appCfg.BaudRate)
	radio = NewQuanshengCAT(appCfg.SerialPort, appCfg.BaudRate)

	log.Printf("[System] Initializing built-in WebRTC server (Pion)...")
	initWebRTC()

	// Initialize MQTT client
	mqttManager.Init(appCfg)

	// Launch background tasks
	go txAudioPlayerLoop()
	go sysMonitor()
	go sMeterPoller()
	go hwScannerTask()
	go governorWatcher()
	go watchdogTOT()

	// Initial switch to background services (APRS / DTMF)
	applyBackgroundServices()
	go publishRadioStatus()

	// Configure HTTP routing
	mux := http.NewServeMux()

	// WebSocket endpoints
	mux.HandleFunc("/tx-ws/", handleWebSocket)
	mux.HandleFunc("/ws", handleWebSocket)

	// WHEP endpoints (for pubhtml/owrx.js and WebRTC players)
	mux.HandleFunc("/webrtc-api/radio/whep", handleWHEP)
	mux.HandleFunc("/whep", handleWHEP)

	// OpenWebRX quick redirect endpoint (redirects to HTTPS proxy port)
	mux.HandleFunc("/owrx/", func(w http.ResponseWriter, r *http.Request) {
		configLock.RLock()
		port := appCfg.OwrxProxyPort
		enabled := appCfg.OwrxProxyEnabled
		configLock.RUnlock()
		if !enabled {
			http.Error(w, "OpenWebRX proxy is disabled in config.json", http.StatusNotFound)
			return
		}
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		target := fmt.Sprintf("https://%s:%d/", host, port)
		http.Redirect(w, r, target, http.StatusTemporaryRedirect)
	})
	mux.HandleFunc("/owrx", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/owrx/", http.StatusMovedPermanently)
	})

	// Live log stream endpoint for browser/tools
	mux.HandleFunc("/logs", handleWebLogs)

	// Static HTML / JS / PWA files
	mux.HandleFunc("/", handleRoot)

	// Handle shutdown signals (Graceful Shutdown)
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigChan
		log.Println("\n[System] Shutting down catWebservice...")
		mqttManager.Disconnect()
		if radio != nil {
			radio.RxOn()
		}
		stopTxAudioProcess()
		governorLock.Lock()
		if direwolfProc != nil && direwolfProc.Process != nil {
			_ = direwolfProc.Process.Kill()
		}
		if rxAudioProc != nil && rxAudioProc.Process != nil {
			_ = rxAudioProc.Process.Kill()
		}
		governorLock.Unlock()
		if activeLogWriter != nil {
			_ = activeLogWriter.Close()
		}
		os.Exit(0)
	}()

	// Check and start HTTPS server
	if _, errC := os.Stat(certPath); errC == nil {
		if _, errK := os.Stat(keyPath); errK == nil {
			httpsPort := appCfg.HttpsPort
			if httpsPort == 0 {
				httpsPort = 8443
			}
			httpsAddr := fmt.Sprintf("%s:%d", appCfg.WsHost, httpsPort)
			go func() {
				log.Printf("[System] HTTPS server started: https://%s:%d (certificate: %s)", appCfg.WsHost, httpsPort, certPath)
				if err := http.ListenAndServeTLS(httpsAddr, certPath, keyPath, mux); err != nil {
					log.Printf("[System] HTTPS server error: %v", err)
				}
			}()

			// Start OpenWebRX HTTPS Reverse Proxy if enabled
			if appCfg.OwrxProxyEnabled {
				startOwrxProxy(certPath, keyPath)
			}
		} else {
			log.Printf("[System] Warning: Private key file %s does not exist, skipping HTTPS server", keyPath)
		}
	} else {
		log.Printf("[System] Warning: Certificate file %s does not exist, skipping HTTPS server", certPath)
	}

	httpAddr := fmt.Sprintf("%s:%d", appCfg.WsHost, appCfg.WsPort)
	log.Printf("[System] HTTP server started: http://%s:%d. Configuration loaded from %s", appCfg.WsHost, appCfg.WsPort, cfgFile)
	log.Fatal(http.ListenAndServe(httpAddr, mux))
}

// startOwrxProxy starts a dedicated HTTPS reverse proxy for OpenWebRX.
// It proxies all web and SDR waterfall WebSocket traffic to OpenWebRX (e.g. 127.0.0.1:8073)
// while providing HTTPS TLS termination, serving owrx.js, and auto-injecting the CAT overlay
// script into OpenWebRX HTML so users get full microphone access and radio controls with zero setup.
func startOwrxProxy(certPath, keyPath string) {
	configLock.RLock()
	targetStr := appCfg.OwrxBackendURL
	proxyPort := appCfg.OwrxProxyPort
	host := appCfg.WsHost
	configLock.RUnlock()

	if targetStr == "" {
		targetStr = "http://127.0.0.1:8073"
	}
	if proxyPort == 0 {
		proxyPort = 8074
	}

	targetURL, err := url.Parse(targetStr)
	if err != nil {
		log.Printf("[OWRX Proxy] Invalid backend URL %s: %v", targetStr, err)
		return
	}

	proxy := httputil.NewSingleHostReverseProxy(targetURL)
	originalDirector := proxy.Director
	proxy.Director = func(req *http.Request) {
		originalDirector(req)
		req.Host = targetURL.Host
		// Remove Accept-Encoding so backend returns plain uncompressed HTML for owrx.js injection
		req.Header.Del("Accept-Encoding")
		if clientIP, _, err := net.SplitHostPort(req.RemoteAddr); err == nil {
			req.Header.Set("X-Forwarded-For", clientIP)
		}
		req.Header.Set("X-Forwarded-Proto", "https")
	}

	proxy.ModifyResponse = func(resp *http.Response) error {
		contentType := resp.Header.Get("Content-Type")
		if strings.Contains(contentType, "text/html") {
			bodyBytes, err := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if err != nil {
				return err
			}

			htmlStr := string(bodyBytes)
			if !strings.Contains(htmlStr, "owrx.js") {
				// Inject script tag loading owrx.js from the same HTTPS proxy origin
				scriptTag := `<script src="/owrx.js"></script>`
				lower := strings.ToLower(htmlStr)
				if idx := strings.LastIndex(lower, "</body>"); idx != -1 {
					htmlStr = htmlStr[:idx] + scriptTag + "\n" + htmlStr[idx:]
				} else if idx := strings.LastIndex(lower, "</head>"); idx != -1 {
					htmlStr = htmlStr[:idx] + scriptTag + "\n" + htmlStr[idx:]
				} else {
					htmlStr = htmlStr + "\n" + scriptTag
				}
			}

			// Strip Content-Security-Policy headers that could block script execution
			resp.Header.Del("Content-Security-Policy")
			resp.Header.Del("Content-Security-Policy-Report-Only")

			newBody := []byte(htmlStr)
			resp.Body = io.NopCloser(bytes.NewReader(newBody))
			resp.ContentLength = int64(len(newBody))
			resp.Header.Set("Content-Length", strconv.Itoa(len(newBody)))
		}
		return nil
	}

	proxyMux := http.NewServeMux()

	// 1. Serve owrx.js overlay directly from disk
	proxyMux.HandleFunc("/owrx.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/javascript")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		http.ServeFile(w, r, filepath.Join(scriptDir, "pubhtml", "owrx.js"))
	})

	// 2. Handlers for CAT WebSocket and WebRTC audio (same-origin with proxy!)
	proxyMux.HandleFunc("/tx-ws/", handleWebSocket)
	proxyMux.HandleFunc("/webrtc-api/radio/whep", handleWHEP)
	proxyMux.HandleFunc("/whep", handleWHEP)

	// 3. Fallback to OpenWebRX backend (handles /static/, /ws/, etc.)
	proxyMux.Handle("/", proxy)

	proxyAddr := fmt.Sprintf("%s:%d", host, proxyPort)

	go func() {
		log.Printf("[OWRX Proxy] OpenWebRX HTTPS Reverse Proxy started: https://%s:%d -> %s (with auto-injected owrx.js)", host, proxyPort, targetStr)
		if err := http.ListenAndServeTLS(proxyAddr, certPath, keyPath, proxyMux); err != nil {
			log.Printf("[OWRX Proxy] Server error: %v", err)
		}
	}()
}

// ensureTLSCertificates checks if the TLS certificate and private key exist.
// If either is missing, it prompts the user on stdin to generate a 10-year self-signed certificate.
func ensureTLSCertificates(certPath, keyPath string) {
	_, errC := os.Stat(certPath)
	_, errK := os.Stat(keyPath)
	if errC == nil && errK == nil {
		return
	}

	var missing []string
	if errC != nil {
		missing = append(missing, certPath)
	}
	if errK != nil {
		missing = append(missing, keyPath)
	}

	fmt.Printf("\n[TLS] Missing TLS file(s): %s\n", strings.Join(missing, ", "))
	fmt.Print("[TLS] Do you want to generate a self-signed TLS certificate valid for 10 years? [Y/n]: ")
	_ = os.Stdout.Sync()

	reader := bufio.NewReader(os.Stdin)
	input, err := reader.ReadString('\n')
	if err != nil && len(strings.TrimSpace(input)) == 0 {
		log.Printf("[TLS] Unable to read confirmation from stdin. Skipping automatic certificate generation.")
		return
	}

	ans := strings.ToLower(strings.TrimSpace(input))
	if ans != "" && ans != "y" && ans != "yes" {
		log.Printf("[TLS] Certificate generation declined by user. HTTPS server will be skipped.")
		return
	}

	log.Printf("[TLS] Generating 10-year self-signed TLS certificate (%s, %s)...", certPath, keyPath)
	if err := generateSelfSignedCert(certPath, keyPath); err != nil {
		log.Printf("[TLS] Error generating certificate: %v", err)
	} else {
		log.Printf("[TLS] Certificate successfully generated and saved to %s and %s", certPath, keyPath)
	}
}

// generateSelfSignedCert creates an RSA private key and self-signed X.509 certificate
// valid for 10 years, configured with SANs for localhost, hostnames, and local IP addresses.
func generateSelfSignedCert(certPath, keyPath string) error {
	// Create directory if it doesn't exist
	if certDir := filepath.Dir(certPath); certDir != "" && certDir != "." {
		if err := os.MkdirAll(certDir, 0755); err != nil {
			return fmt.Errorf("failed to create directory %s: %w", certDir, err)
		}
	}
	if keyDir := filepath.Dir(keyPath); keyDir != "" && keyDir != "." {
		if err := os.MkdirAll(keyDir, 0755); err != nil {
			return fmt.Errorf("failed to create directory %s: %w", keyDir, err)
		}
	}

	priv, err := rsa.GenerateKey(crand.Reader, 2048)
	if err != nil {
		return fmt.Errorf("failed to generate RSA key: %w", err)
	}

	serialNumberLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serialNumber, err := crand.Int(crand.Reader, serialNumberLimit)
	if err != nil {
		return fmt.Errorf("failed to generate serial number: %w", err)
	}

	now := time.Now()
	notBefore := now.Add(-1 * time.Hour) // clock skew tolerance
	notAfter := now.AddDate(10, 0, 0)     // valid for 10 years

	template := x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			Organization: []string{"Quansheng CAT WebService"},
			CommonName:   "Quansheng CAT WebService",
		},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
	}

	if host, err := os.Hostname(); err == nil && host != "" {
		template.DNSNames = append(template.DNSNames, host)
	}

	if appCfg.WsHost != "" && appCfg.WsHost != "0.0.0.0" {
		if ip := net.ParseIP(appCfg.WsHost); ip != nil {
			template.IPAddresses = append(template.IPAddresses, ip)
		} else {
			template.DNSNames = append(template.DNSNames, appCfg.WsHost)
		}
	}

	// Add local network interface IPs to Subject Alternative Names (SAN)
	if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, addr := range addrs {
			if ipnet, ok := addr.(*net.IPNet); ok && !ipnet.IP.IsLoopback() {
				if ip4 := ipnet.IP.To4(); ip4 != nil {
					template.IPAddresses = append(template.IPAddresses, ip4)
				}
			}
		}
	}

	derBytes, err := x509.CreateCertificate(crand.Reader, &template, &template, &priv.PublicKey, priv)
	if err != nil {
		return fmt.Errorf("failed to create certificate: %w", err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: derBytes})
	if err := os.WriteFile(certPath, certPEM, 0644); err != nil {
		return fmt.Errorf("failed to write certificate to %s: %w", certPath, err)
	}

	privBytes, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return fmt.Errorf("failed to marshal private key: %w", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privBytes})
	if err := os.WriteFile(keyPath, keyPEM, 0600); err != nil {
		return fmt.Errorf("failed to write private key to %s: %w", keyPath, err)
	}

	return nil
}