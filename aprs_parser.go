package main

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

type AprsWeather struct {
	WindMph        *float64 `json:"wind_mph,omitempty"`
	WindKmh        *float64 `json:"wind_kmh,omitempty"`
	WindDir        *int     `json:"wind_dir,omitempty"`
	GustMph        *float64 `json:"gust_mph,omitempty"`
	GustKmh        *float64 `json:"gust_kmh,omitempty"`
	TempF          *float64 `json:"temp_f,omitempty"`
	TempC          *float64 `json:"temp_c,omitempty"`
	Humidity       *int     `json:"humidity,omitempty"`
	BarometerInHg  *float64 `json:"barometer_inhg,omitempty"`
	BarometerHPa   *float64 `json:"barometer_hpa,omitempty"`
	Rain1hIn       *float64 `json:"rain_1h_in,omitempty"`
	Rain1hMm       *float64 `json:"rain_1h_mm,omitempty"`
	Rain24hIn      *float64 `json:"rain_24h_in,omitempty"`
	Rain24hMm      *float64 `json:"rain_24h_mm,omitempty"`
	RainMidnightIn *float64 `json:"rain_midnight_in,omitempty"`
	RainMidnightMm *float64 `json:"rain_midnight_mm,omitempty"`
	SolarWm2       *float64 `json:"solar_w_m2,omitempty"`
}

type AprsTelemetry struct {
	Seq int            `json:"seq"`
	A1  int            `json:"a1"`
	A2  int            `json:"a2"`
	A3  int            `json:"a3"`
	A4  int            `json:"a4"`
	A5  int            `json:"a5"`
	D1  int            `json:"d1"`
	D2  int            `json:"d2"`
	D3  int            `json:"d3"`
	D4  int            `json:"d4"`
	D5  int            `json:"d5"`
	D6  int            `json:"d6"`
	D7  int            `json:"d7"`
	D8  int            `json:"d8"`
	Raw map[string]any `json:"raw,omitempty"`
}

type AprsPacket struct {
	Timestamp     string         `json:"timestamp"`
	Source        string         `json:"source"`
	Destination   string         `json:"destination"`
	Path          []string       `json:"path"`
	Heard         string         `json:"heard,omitempty"`
	HeardDigi     string         `json:"heard_digi,omitempty"`
	Channel       string         `json:"channel,omitempty"`
	AudioLevel    int            `json:"audio_level,omitempty"`
	AudioLevelSub string         `json:"audio_level_sub,omitempty"`
	AudioWarning  string         `json:"audio_warning,omitempty"`
	PacketType    string         `json:"packet_type,omitempty"`
	SymbolDesc    string         `json:"symbol_desc,omitempty"`
	DeviceInfo    string         `json:"device_info,omitempty"`
	Status        string         `json:"status,omitempty"`
	Latitude      *float64       `json:"latitude,omitempty"`
	Longitude     *float64       `json:"longitude,omitempty"`
	SpeedKmh      *float64       `json:"speed_kmh,omitempty"`
	SpeedMph      *float64       `json:"speed_mph,omitempty"`
	Course        *int           `json:"course,omitempty"`
	AltitudeM     *float64       `json:"altitude_m,omitempty"`
	AltitudeFt    *float64       `json:"altitude_ft,omitempty"`
	FreqMHz       *float64       `json:"freq_mhz,omitempty"`
	Comment       string         `json:"comment,omitempty"`
	Payload       string         `json:"payload"`
	RawFrame      string         `json:"raw_frame"`
	Weather       *AprsWeather   `json:"weather,omitempty"`
	Telemetry     *AprsTelemetry `json:"telemetry,omitempty"`
}

var (
	ansiRegex     = regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]`)
	frameRegex    = regexp.MustCompile(`^\[([0-9.]+)\]\s+([A-Za-z0-9-]+)>([A-Za-z0-9-]+)((?:,[A-Za-z0-9*-]+)*):(.*)$`)
	audioLvlRegex = regexp.MustCompile(`(?i)(?:Digipeater\s+([^\s]+(?:\s+\(probably\s+[^)]+\))?)|([^\s]+))\s+audio level\s*=\s*(\d+)(?:\(([^)]+)\))?`)
	posRegex      = regexp.MustCompile(`([NS])\s+(\d+)\s+([\d.]+),\s+([EW])\s+(\d+)\s+([\d.]+)`)
	speedRegex    = regexp.MustCompile(`(\d+(?:\.\d+)?)\s*km/h\s*\((\d+(?:\.\d+)?)\s*MPH\)`)
	courseRegex   = regexp.MustCompile(`course\s*(\d+)`)
	altRegex      = regexp.MustCompile(`alt\s*(\d+(?:\.\d+)?)\s*m\s*\((\d+(?:\.\d+)?)\s*ft\)`)
	freqRegex     = regexp.MustCompile(`(\d{3}\.\d{3,4})\s*MHz`)
	rawPosRegex   = regexp.MustCompile(`([0-9]{2})([0-9]{2}\.[0-9]{2})([NS])[\/\\]([0-9]{3})([0-9]{2}\.[0-9]{2})([EW])`)
)

type DirewolfStreamParser struct {
	lock         sync.Mutex
	currentLines []string
	flushTimer   *time.Timer
	onPacket     func(pkt AprsPacket)
}

func NewDirewolfStreamParser(onPacket func(pkt AprsPacket)) *DirewolfStreamParser {
	return &DirewolfStreamParser{
		onPacket: onPacket,
	}
}

func (p *DirewolfStreamParser) FeedLine(rawLine string) {
	clean := ansiRegex.ReplaceAllString(rawLine, "")
	clean = strings.TrimSpace(clean)

	p.lock.Lock()
	defer p.lock.Unlock()

	// Detect the start of a new frame or a new audio level header
	isNewHeader := strings.Contains(clean, "audio level =") || (strings.HasPrefix(clean, "[") && frameRegex.MatchString(clean))

	if isNewHeader {
		// If we already have a complete frame buffered, flush it immediately and clear the buffer
		if p.hasFrameLineLocked() {
			p.flushLocked()
		}
	}

	if clean == "" {
		// Empty line after a received frame signals end of block
		if p.hasFrameLineLocked() {
			p.flushLocked()
		}
		return
	}

	p.currentLines = append(p.currentLines, clean)

	// Reset auto-flush timer (200ms)
	if p.flushTimer != nil {
		p.flushTimer.Stop()
	}
	p.flushTimer = time.AfterFunc(200*time.Millisecond, func() {
		p.lock.Lock()
		defer p.lock.Unlock()
		if p.hasFrameLineLocked() {
			p.flushLocked()
		}
	})
}

func (p *DirewolfStreamParser) hasFrameLineLocked() bool {
	for _, l := range p.currentLines {
		if strings.HasPrefix(l, "[") && frameRegex.MatchString(l) {
			return true
		}
	}
	return false
}

func (p *DirewolfStreamParser) flushLocked() {
	if len(p.currentLines) == 0 {
		return
	}
	lines := p.currentLines
	p.currentLines = nil
	if p.flushTimer != nil {
		p.flushTimer.Stop()
		p.flushTimer = nil
	}

	go func(b []string) {
		pkt, ok := parsePacketBlock(b)
		if ok && p.onPacket != nil {
			p.onPacket(pkt)
		}
	}(lines)
}

func parsePacketBlock(lines []string) (AprsPacket, bool) {
	var pkt AprsPacket
	pkt.Timestamp = time.Now().UTC().Format(time.RFC3339)
	foundFrame := false

	for _, line := range lines {
		// 1. Audio level line
		if strings.Contains(line, "audio level =") {
			m := audioLvlRegex.FindStringSubmatch(line)
			if len(m) >= 4 {
				h := m[1]
				if h == "" {
					h = m[2]
				}
				pkt.Heard = strings.TrimSpace(h)
				if lvl, err := strconv.Atoi(m[3]); err == nil {
					pkt.AudioLevel = lvl
				}
				if len(m) >= 5 && m[4] != "" {
					pkt.AudioLevelSub = m[4]
				}
			}
		}

		// 2. Audio warning
		if strings.HasPrefix(line, "Audio input level is too high") {
			pkt.AudioWarning = line
		}

		// 3. AX.25 frame: [0.5] SRC>DST,PATH:PAYLOAD
		if strings.HasPrefix(line, "[") {
			mf := frameRegex.FindStringSubmatch(line)
			if len(mf) >= 6 {
				foundFrame = true
				pkt.Channel = mf[1]
				pkt.Source = mf[2]
				pkt.Destination = mf[3]
				pkt.Payload = mf[5]
				pkt.RawFrame = fmt.Sprintf("%s>%s%s:%s", mf[2], mf[3], mf[4], mf[5])

				pathStr := strings.TrimPrefix(mf[4], ",")
				if pathStr != "" {
					parts := strings.Split(pathStr, ",")
					for _, part := range parts {
						pTrim := strings.TrimSpace(part)
						if pTrim != "" {
							pkt.Path = append(pkt.Path, pTrim)
							if strings.HasSuffix(pTrim, "*") {
								pkt.HeardDigi = strings.TrimSuffix(pTrim, "*")
							}
						}
					}
				}
			}
		}

		// 4. Position line: N 52 24.7800, E 016 53.5200 ...
		if strings.HasPrefix(line, "N ") || strings.HasPrefix(line, "S ") {
			mpos := posRegex.FindStringSubmatch(line)
			if len(mpos) >= 7 {
				latDeg, _ := strconv.ParseFloat(mpos[2], 64)
				latMin, _ := strconv.ParseFloat(mpos[3], 64)
				lat := latDeg + latMin/60.0
				if mpos[1] == "S" {
					lat = -lat
				}

				lonDeg, _ := strconv.ParseFloat(mpos[5], 64)
				lonMin, _ := strconv.ParseFloat(mpos[6], 64)
				lon := lonDeg + lonMin/60.0
				if mpos[4] == "W" {
					lon = -lon
				}

				latRounded := math.Round(lat*1000000) / 1000000
				lonRounded := math.Round(lon*1000000) / 1000000
				pkt.Latitude = &latRounded
				pkt.Longitude = &lonRounded
			}

			// Motion parameters on position line
			mspeed := speedRegex.FindStringSubmatch(line)
			if len(mspeed) >= 3 {
				if kmh, err := strconv.ParseFloat(mspeed[1], 64); err == nil {
					pkt.SpeedKmh = &kmh
				}
				if mph, err := strconv.ParseFloat(mspeed[2], 64); err == nil {
					pkt.SpeedMph = &mph
				}
			}

			mcourse := courseRegex.FindStringSubmatch(line)
			if len(mcourse) >= 2 {
				if c, err := strconv.Atoi(mcourse[1]); err == nil {
					pkt.Course = &c
				}
			}

			malt := altRegex.FindStringSubmatch(line)
			if len(malt) >= 3 {
				if m, err := strconv.ParseFloat(malt[1], 64); err == nil {
					pkt.AltitudeM = &m
				}
				if ft, err := strconv.ParseFloat(malt[2], 64); err == nil {
					pkt.AltitudeFt = &ft
				}
			}

			mfreq := freqRegex.FindStringSubmatch(line)
			if len(mfreq) >= 2 {
				if f, err := strconv.ParseFloat(mfreq[1], 64); err == nil {
					pkt.FreqMHz = &f
				}
			}
		}

		// 5. Telemetry line: Seq=809, A1=137, A2=20, A3=255, A4=246, A5=217, D1=0...
		if strings.HasPrefix(line, "Seq=") {
			telem := &AprsTelemetry{Raw: make(map[string]any)}
			parts := strings.Split(line, ",")
			for _, part := range parts {
				kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
				if len(kv) == 2 {
					k := strings.ToLower(strings.TrimSpace(kv[0]))
					valStr := strings.TrimSpace(kv[1])
					valInt, err := strconv.Atoi(valStr)
					if err == nil {
						telem.Raw[k] = valInt
						switch k {
						case "seq":
							telem.Seq = valInt
						case "a1":
							telem.A1 = valInt
						case "a2":
							telem.A2 = valInt
						case "a3":
							telem.A3 = valInt
						case "a4":
							telem.A4 = valInt
						case "a5":
							telem.A5 = valInt
						case "d1":
							telem.D1 = valInt
						case "d2":
							telem.D2 = valInt
						case "d3":
							telem.D3 = valInt
						case "d4":
							telem.D4 = valInt
						case "d5":
							telem.D5 = valInt
						case "d6":
							telem.D6 = valInt
						case "d7":
							telem.D7 = valInt
						case "d8":
							telem.D8 = valInt
						}
					} else {
						telem.Raw[k] = valStr
					}
				}
			}
			pkt.Telemetry = telem
		}

		// 6. Weather line (Weather Report)
		if strings.Contains(line, "wind ") && strings.Contains(line, "temperature ") {
			wx := &AprsWeather{}

			rWind := regexp.MustCompile(`wind\s*([\d.]+)\s*mph`)
			if m := rWind.FindStringSubmatch(line); len(m) >= 2 {
				if v, err := strconv.ParseFloat(m[1], 64); err == nil {
					wx.WindMph = &v
					kmh := math.Round(v*1.60934*10) / 10
					wx.WindKmh = &kmh
				}
			}

			rDir := regexp.MustCompile(`direction\s*(\d+)`)
			if m := rDir.FindStringSubmatch(line); len(m) >= 2 {
				if v, err := strconv.Atoi(m[1]); err == nil {
					wx.WindDir = &v
				}
			}

			rGust := regexp.MustCompile(`gust\s*([\d.]+)`)
			if m := rGust.FindStringSubmatch(line); len(m) >= 2 {
				if v, err := strconv.ParseFloat(m[1], 64); err == nil {
					wx.GustMph = &v
					kmh := math.Round(v*1.60934*10) / 10
					wx.GustKmh = &kmh
				}
			}

			rTemp := regexp.MustCompile(`temperature\s*(-?[\d.]+)`)
			if m := rTemp.FindStringSubmatch(line); len(m) >= 2 {
				if v, err := strconv.ParseFloat(m[1], 64); err == nil {
					wx.TempF = &v
					c := math.Round(((v-32.0)*5.0/9.0)*10) / 10
					wx.TempC = &c
				}
			}

			rHum := regexp.MustCompile(`humidity\s*(\d+)`)
			if m := rHum.FindStringSubmatch(line); len(m) >= 2 {
				if v, err := strconv.Atoi(m[1]); err == nil {
					wx.Humidity = &v
				}
			}

			rBaro := regexp.MustCompile(`barometer\s*([\d.]+)`)
			if m := rBaro.FindStringSubmatch(line); len(m) >= 2 {
				if v, err := strconv.ParseFloat(m[1], 64); err == nil {
					wx.BarometerInHg = &v
					hpa := math.Round(v*33.863886666667*10) / 10
					wx.BarometerHPa = &hpa
				}
			}

			rRain1h := regexp.MustCompile(`rain\s*([\d.]+)\s*in last hour`)
			if m := rRain1h.FindStringSubmatch(line); len(m) >= 2 {
				if v, err := strconv.ParseFloat(m[1], 64); err == nil {
					wx.Rain1hIn = &v
					mm := math.Round(v*25.4*10) / 10
					wx.Rain1hMm = &mm
				}
			}

			rRain24h := regexp.MustCompile(`rain\s*([\d.]+)\s*in last 24 hours`)
			if m := rRain24h.FindStringSubmatch(line); len(m) >= 2 {
				if v, err := strconv.ParseFloat(m[1], 64); err == nil {
					wx.Rain24hIn = &v
					mm := math.Round(v*25.4*10) / 10
					wx.Rain24hMm = &mm
				}
			}

			rRainMid := regexp.MustCompile(`rain\s*([\d.]+)\s*since midnight`)
			if m := rRainMid.FindStringSubmatch(line); len(m) >= 2 {
				if v, err := strconv.ParseFloat(m[1], 64); err == nil {
					wx.RainMidnightIn = &v
					mm := math.Round(v*25.4*10) / 10
					wx.RainMidnightMm = &mm
				}
			}

			rSolar := regexp.MustCompile(`([\d.]+)\s*watts/m\^2`)
			if m := rSolar.FindStringSubmatch(line); len(m) >= 2 {
				if v, err := strconv.ParseFloat(m[1], 64); err == nil {
					wx.SolarWm2 = &v
				}
			}

			rComm := regexp.MustCompile(`"([^"]*)"`)
			if m := rComm.FindStringSubmatch(line); len(m) >= 2 {
				comm := strings.TrimSpace(m[1])
				if comm != "" {
					pkt.Comment = comm
				}
			}

			pkt.Weather = wx
		}

		// 7. Packet classification (MIC-E, Weather Report, Position, etc.)
		firstTokens := []string{"MIC-E", "Weather Report", "Position with time", "Position", "Telemetry", "Object", "Item"}
		for _, tok := range firstTokens {
			if strings.HasPrefix(line, tok) && !strings.HasPrefix(line, "Position:") {
				parts := strings.Split(line, ",")
				pkt.PacketType = strings.TrimSpace(parts[0])
				if len(parts) > 1 {
					pkt.SymbolDesc = strings.TrimSpace(parts[1])
				}
				if len(parts) > 2 {
					pkt.DeviceInfo = strings.TrimSpace(parts[2])
				}
				if len(parts) > 3 {
					pkt.Status = strings.TrimSpace(parts[3])
				}
				break
			}
		}

		// 8. Comment
		if pkt.Comment == "" {
			if strings.HasPrefix(line, `, "`) && strings.HasSuffix(line, `"`) {
				pkt.Comment = strings.Trim(line[2:], `" `)
			} else if strings.HasPrefix(line, `"`) && strings.HasSuffix(line, `"`) && len(line) > 2 {
				pkt.Comment = strings.Trim(line, `" `)
			} else if !anyPrefix(line, []string{"Digipeater", "[", "ERROR", "Audio", "Didn't", "Weather", "Position", "MIC-E", "Telemetry", "N ", "S ", "wind ", "Seq=", "Use of", "Tell the", "\"", "'"}) {
				if len(line) > 1 && !strings.Contains(line, "audio level =") {
					pkt.Comment = strings.TrimSpace(line)
				}
			}
		}
	}

	// 9. Fallback: parse coordinates directly from payload if no position line found
	if pkt.Latitude == nil && pkt.Payload != "" {
		m := rawPosRegex.FindStringSubmatch(pkt.Payload)
		if len(m) >= 7 {
			latDeg, _ := strconv.ParseFloat(m[1], 64)
			latMin, _ := strconv.ParseFloat(m[2], 64)
			lat := latDeg + latMin/60.0
			if m[3] == "S" {
				lat = -lat
			}

			lonDeg, _ := strconv.ParseFloat(m[4], 64)
			lonMin, _ := strconv.ParseFloat(m[5], 64)
			lon := lonDeg + lonMin/60.0
			if m[6] == "W" {
				lon = -lon
			}

			latRounded := math.Round(lat*1000000) / 1000000
			lonRounded := math.Round(lon*1000000) / 1000000
			pkt.Latitude = &latRounded
			pkt.Longitude = &lonRounded
		}
	}

	return pkt, foundFrame
}

func anyPrefix(s string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

var globalAprsParser = NewDirewolfStreamParser(func(pkt AprsPacket) {
	// 1. Publish to MQTT broker
	mqttManager.PublishAprs(pkt)

	// 2. Send as event to active WebSocket clients (e.g. web panel)
	wsData, err := json.Marshal(map[string]interface{}{
		"cmd":    "aprs_packet",
		"packet": pkt,
	})
	if err == nil {
		dispatchToClients(wsData)
	}
})

