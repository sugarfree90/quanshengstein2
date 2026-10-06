package main

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
)

type MQTTManager struct {
	client    mqtt.Client
	connected bool
	lock      sync.RWMutex
	cfg       Config

	smeterLock      sync.Mutex
	latestSMeter    *SMeterReport
	smeterNotify    chan struct{}
	workerStop      chan struct{}
	workerDone      chan struct{}
	isWorkerRunning bool
}

var mqttManager = &MQTTManager{}

func (m *MQTTManager) Init(cfg Config) {
	m.lock.Lock()
	m.cfg = cfg
	if m.smeterNotify == nil {
		m.smeterNotify = make(chan struct{}, 1)
	}
	enabled := cfg.MQTTAPREnabled || cfg.MQTTDTMFEnabled || cfg.MQTTStatusEnabled || cfg.MQTTSMeterEnabled || cfg.MQTTEnabled
	broker := cfg.MQTTBroker
	m.lock.Unlock()

	if !enabled || strings.TrimSpace(broker) == "" {
		m.Disconnect()
		log.Println("[MQTT] MQTT client support is disabled in configuration.")
		return
	}

	m.Connect()
}

func (m *MQTTManager) Connect() {
	m.lock.Lock()
	defer m.lock.Unlock()

	if m.client != nil && m.client.IsConnected() {
		m.client.Disconnect(250)
		m.client = nil
		m.connected = false
	}

	broker := strings.TrimSpace(m.cfg.MQTTBroker)
	if broker == "" {
		return
	}

	if !strings.Contains(broker, "://") {
		broker = "tcp://" + broker
	}

	clientID := strings.TrimSpace(m.cfg.MQTTClientID)
	if clientID == "" {
		clientID = fmt.Sprintf("quansheng_%d", time.Now().UnixNano()%100000)
	}

	opts := mqtt.NewClientOptions()
	opts.AddBroker(broker)
	opts.SetClientID(clientID)
	opts.SetAutoReconnect(true)
	opts.SetConnectRetry(true)
	opts.SetConnectRetryInterval(5 * time.Second)
	opts.SetKeepAlive(30 * time.Second)
	opts.SetPingTimeout(10 * time.Second)
	opts.SetCleanSession(true)

	if m.cfg.MQTTUsername != "" {
		opts.SetUsername(m.cfg.MQTTUsername)
		opts.SetPassword(m.cfg.MQTTPassword)
	}

	u, err := url.Parse(broker)
	if err == nil && (u.Scheme == "ssl" || u.Scheme == "tls" || u.Scheme == "tcps") {
		opts.SetTLSConfig(&tls.Config{InsecureSkipVerify: true})
	}

	opts.OnConnect = func(c mqtt.Client) {
		m.lock.Lock()
		m.connected = true
		m.lock.Unlock()
		log.Printf("[MQTT] Successfully connected to broker: %s (ClientID: %s)", broker, clientID)
		m.startSMeterWorker()
		go publishRadioStatus()
	}

	opts.OnConnectionLost = func(c mqtt.Client, err error) {
		m.lock.Lock()
		m.connected = false
		m.lock.Unlock()
		log.Printf("[MQTT] Connection lost to broker %s: %v", broker, err)
		m.stopSMeterWorker()
	}

	client := mqtt.NewClient(opts)
	m.client = client

	go func() {
		log.Printf("[MQTT] Connecting to broker: %s ...", broker)
		token := client.Connect()
		if token.WaitTimeout(10 * time.Second) && token.Error() != nil {
			log.Printf("[MQTT] Initial connection attempt failed (auto-reconnect active in background): %v", token.Error())
		}
	}()
}

func (m *MQTTManager) Disconnect() {
	m.stopSMeterWorker()
	m.lock.Lock()
	defer m.lock.Unlock()
	if m.client != nil {
		if m.client.IsConnected() {
			m.client.Disconnect(250)
		}
		m.client = nil
	}
	m.connected = false
}

func (m *MQTTManager) IsConnected() bool {
	m.lock.RLock()
	defer m.lock.RUnlock()
	return m.client != nil && m.client.IsConnected()
}

func (m *MQTTManager) UpdateConfig(cfg Config) {
	m.lock.Lock()
	m.cfg = cfg
	enabled := cfg.MQTTAPREnabled || cfg.MQTTDTMFEnabled || cfg.MQTTStatusEnabled || cfg.MQTTSMeterEnabled || cfg.MQTTEnabled
	m.lock.Unlock()

	if !enabled {
		m.Disconnect()
		log.Println("[MQTT] MQTT support disabled.")
	} else {
		log.Println("[MQTT] MQTT settings updated, reconnecting...")
		m.Connect()
	}
}

func (m *MQTTManager) Publish(topic string, qos byte, retained bool, payload interface{}) error {
	m.lock.RLock()
	client := m.client
	m.lock.RUnlock()

	if client == nil || !client.IsConnected() {
		return nil
	}

	var data []byte
	switch v := payload.(type) {
	case []byte:
		data = v
	case string:
		data = []byte(v)
	default:
		var err error
		data, err = json.Marshal(v)
		if err != nil {
			return err
		}
	}

	token := client.Publish(topic, qos, retained, data)
	go func() {
		if token.WaitTimeout(3 * time.Second) && token.Error() != nil {
			log.Printf("[MQTT] Publish error to %s: %v", topic, token.Error())
		}
	}()
	return nil
}

func (m *MQTTManager) PublishAprs(pkt AprsPacket) {
	m.lock.RLock()
	enabled := m.cfg.MQTTAPREnabled || m.cfg.MQTTEnabled
	aprsTopic := m.cfg.MQTTAPRSTopic
	if aprsTopic == "" {
		aprsTopic = m.cfg.MQTTTopicPrefix
	}
	retain := m.cfg.MQTTRetain
	qos := byte(m.cfg.MQTTQoS)
	m.lock.RUnlock()

	if !enabled || !m.IsConnected() {
		return
	}

	if aprsTopic == "" {
		aprsTopic = "aprs"
	}

	// 1. All packets to general topic
	allTopic := fmt.Sprintf("%s/packets", aprsTopic)
	_ = m.Publish(allTopic, qos, retain, pkt)

	// 2. Dedicated station topic
	if pkt.Source != "" {
		stationTopic := fmt.Sprintf("%s/stations/%s", aprsTopic, pkt.Source)
		_ = m.Publish(stationTopic, qos, retain, pkt)
	}

	// 3. Dedicated weather topic (WX)
	if pkt.Weather != nil && pkt.Source != "" {
		wxTopic := fmt.Sprintf("%s/weather/%s", aprsTopic, pkt.Source)
		_ = m.Publish(wxTopic, qos, retain, pkt.Weather)
	}

	// 4. Dedicated telemetry topic
	if pkt.Telemetry != nil && pkt.Source != "" {
		telemTopic := fmt.Sprintf("%s/telemetry/%s", aprsTopic, pkt.Source)
		_ = m.Publish(telemTopic, qos, retain, pkt.Telemetry)
	}

	// 5. GPS position for Home Assistant / Device Tracker
	if pkt.Latitude != nil && pkt.Longitude != nil && pkt.Source != "" {
		posTopic := fmt.Sprintf("%s/positions/%s", aprsTopic, pkt.Source)
		posPayload := map[string]interface{}{
			"source":    pkt.Source,
			"latitude":  *pkt.Latitude,
			"longitude": *pkt.Longitude,
			"timestamp": pkt.Timestamp,
		}
		if pkt.SpeedKmh != nil {
			posPayload["speed_kmh"] = *pkt.SpeedKmh
		}
		if pkt.Course != nil {
			posPayload["course"] = *pkt.Course
		}
		if pkt.AltitudeM != nil {
			posPayload["altitude_m"] = *pkt.AltitudeM
		}
		_ = m.Publish(posTopic, qos, retain, posPayload)
	}

	log.Printf("[MQTT] Published APRS packet from %s (type: %s) -> %s/stations/%s", pkt.Source, pkt.PacketType, aprsTopic, pkt.Source)
}

func (m *MQTTManager) PublishDTMF(report DTMFReport) {
	m.lock.RLock()
	enabled := m.cfg.MQTTDTMFEnabled
	dtmfTopic := m.cfg.MQTTDTMFTopic
	retain := m.cfg.MQTTRetain
	qos := byte(m.cfg.MQTTQoS)
	m.lock.RUnlock()

	if !enabled || !m.IsConnected() {
		return
	}

	if dtmfTopic == "" {
		dtmfTopic = "dtmf"
	}

	_ = m.Publish(dtmfTopic, qos, retain, report)
	log.Printf("[MQTT] Published DTMF report '%s' (dBm: %d) -> %s", report.Code, report.Dbm, dtmfTopic)
}

type RadioStatusReport struct {
	Timestamp   string  `json:"timestamp"`
	Callsign    string  `json:"callsign,omitempty"`
	PTT         bool    `json:"ptt"`
	Freq        int     `json:"freq"`
	FreqMHz     float64 `json:"freq_mhz"`
	Mod         string  `json:"mod"`
	Pwr         int     `json:"pwr"`
	Ctcss       float64 `json:"ctcss"`
	Dcs         int     `json:"dcs"`
	ShiftDir    int     `json:"shift_dir"`
	ShiftVal    float64 `json:"shift_val"`
	TxFreq      int     `json:"tx_freq"`
	TxFreqMHz   float64 `json:"tx_freq_mhz"`
	Monitor     int     `json:"monitor"`
	SquelchOpen bool    `json:"squelch_open"`
	Squelch     bool    `json:"squelch"`
}

func (m *MQTTManager) PublishRadioStatus(st RadioState, ptt bool, squelch bool, callsign string) {
	m.lock.RLock()
	enabled := m.cfg.MQTTStatusEnabled
	statusTopic := m.cfg.MQTTStatusTopic
	retain := m.cfg.MQTTRetain
	qos := byte(m.cfg.MQTTQoS)
	m.lock.RUnlock()

	if !enabled || !m.IsConnected() {
		return
	}

	if statusTopic == "" {
		statusTopic = "radio/status"
	}

	txFreq := st.Freq
	if !ptt {
		if st.ShiftDir == 1 {
			txFreq = st.Freq + int(math.Round(st.ShiftVal*1000000))
		} else if st.ShiftDir == 2 {
			txFreq = st.Freq - int(math.Round(st.ShiftVal*1000000))
		}
	}

	report := RadioStatusReport{
		Timestamp:   time.Now().UTC().Format(time.RFC3339),
		Callsign:    callsign,
		PTT:         ptt,
		Freq:        st.Freq,
		FreqMHz:     math.Round(float64(st.Freq)/1000.0) / 1000.0,
		Mod:         st.Mod,
		Pwr:         st.Pwr,
		Ctcss:       st.Ctcss,
		Dcs:         st.Dcs,
		ShiftDir:    st.ShiftDir,
		ShiftVal:    st.ShiftVal,
		TxFreq:      txFreq,
		TxFreqMHz:   math.Round(float64(txFreq)/1000.0) / 1000.0,
		Monitor:     st.Monitor,
		SquelchOpen: squelch,
		Squelch:     squelch,
	}

	_ = m.Publish(statusTopic, qos, retain, report)
	log.Printf("[MQTT] Published radio status (PTT: %v, Squelch: %v, Freq: %d Hz, Mod: %s) -> %s", ptt, squelch, st.Freq, st.Mod, statusTopic)
}

type SMeterReport struct {
	Timestamp string  `json:"timestamp"`
	Dbm       int     `json:"dbm"`
	Sql       int     `json:"sql"`
	Squelch   bool    `json:"squelch"`
	Freq      int     `json:"freq,omitempty"`
	FreqMHz   float64 `json:"freq_mhz,omitempty"`
}

func (m *MQTTManager) startSMeterWorker() {
	m.smeterLock.Lock()
	if m.isWorkerRunning {
		m.smeterLock.Unlock()
		return
	}
	if m.smeterNotify == nil {
		m.smeterNotify = make(chan struct{}, 1)
	}
	m.isWorkerRunning = true
	m.workerStop = make(chan struct{})
	m.workerDone = make(chan struct{})
	stopCh := m.workerStop
	doneCh := m.workerDone
	m.smeterLock.Unlock()

	go func() {
		defer func() {
			m.smeterLock.Lock()
			m.isWorkerRunning = false
			m.smeterLock.Unlock()
			close(doneCh)
		}()

		for {
			select {
			case <-stopCh:
				return
			case <-m.smeterNotify:
				// Single-slot conflation / flush: extract latest value and reset slot
				m.smeterLock.Lock()
				report := m.latestSMeter
				m.latestSMeter = nil
				m.smeterLock.Unlock()

				if report == nil {
					continue
				}

				select {
				case <-stopCh:
					return
				default:
				}

				m.publishSMeterSync(*report)
			}
		}
	}()
}

func (m *MQTTManager) stopSMeterWorker() {
	m.smeterLock.Lock()
	if !m.isWorkerRunning {
		m.smeterLock.Unlock()
		return
	}
	m.isWorkerRunning = false
	close(m.workerStop)
	doneCh := m.workerDone
	m.smeterLock.Unlock()

	select {
	case <-doneCh:
	case <-time.After(350 * time.Millisecond):
	}
}

func (m *MQTTManager) PublishSMeter(report SMeterReport) {
	m.lock.RLock()
	enabled := m.cfg.MQTTSMeterEnabled
	connected := m.connected && m.client != nil && m.client.IsConnected()
	m.lock.RUnlock()

	if !enabled || !connected {
		return
	}

	m.smeterLock.Lock()
	if !m.isWorkerRunning {
		m.smeterLock.Unlock()
		m.startSMeterWorker()
		m.smeterLock.Lock()
	}
	if m.smeterNotify == nil {
		m.smeterNotify = make(chan struct{}, 1)
	}
	// Always store only the newest reading; previous unsent reading is automatically flushed/dropped
	m.latestSMeter = &report
	m.smeterLock.Unlock()

	// Non-blocking trigger signal
	select {
	case m.smeterNotify <- struct{}{}:
	default:
		// Worker is already notified and will grab m.latestSMeter when ready
	}
}

func (m *MQTTManager) publishSMeterSync(report SMeterReport) {
	m.lock.RLock()
	client := m.client
	if client == nil || !client.IsConnected() {
		m.lock.RUnlock()
		return
	}
	topic := strings.TrimSpace(m.cfg.MQTTSMeterTopic)
	if topic == "" {
		topic = "radio/smeter"
	}
	retain := m.cfg.MQTTRetain
	qos := byte(m.cfg.MQTTQoS)
	m.lock.RUnlock()

	if report.Freq > 0 && report.FreqMHz == 0 {
		report.FreqMHz = math.Round(float64(report.Freq)/1000.0) / 1000.0
	}

	data, err := json.Marshal(report)
	if err != nil {
		return
	}

	// 1. Publish structured JSON report with dBm, sql, freq, timestamp
	token := client.Publish(topic, qos, retain, data)
	if token.WaitTimeout(300 * time.Millisecond) && token.Error() != nil {
		log.Printf("[MQTT] S-Meter publish error to %s: %v", topic, token.Error())
		return
	}

	// 2. Also publish raw integer dBm to <topic>/raw for microcontroller OLED / LCD displays
	rawTopic := fmt.Sprintf("%s/raw", topic)
	tokenRaw := client.Publish(rawTopic, qos, retain, []byte(strconv.Itoa(report.Dbm)))
	_ = tokenRaw.WaitTimeout(100 * time.Millisecond)
}


