package main

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/url"
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
}

var mqttManager = &MQTTManager{}

func (m *MQTTManager) Init(cfg Config) {
	m.lock.Lock()
	m.cfg = cfg
	enabled := cfg.MQTTAPREnabled || cfg.MQTTDTMFEnabled || cfg.MQTTStatusEnabled || cfg.MQTTEnabled
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
		go publishRadioStatus()
	}

	opts.OnConnectionLost = func(c mqtt.Client, err error) {
		m.lock.Lock()
		m.connected = false
		m.lock.Unlock()
		log.Printf("[MQTT] Connection lost to broker %s: %v", broker, err)
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
	enabled := cfg.MQTTAPREnabled || cfg.MQTTDTMFEnabled || cfg.MQTTStatusEnabled || cfg.MQTTEnabled
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


