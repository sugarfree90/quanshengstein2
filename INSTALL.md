# Installation and Deployment Guide for Quanshengstein CAT Webservice

A comprehensive, step-by-step installation and deployment guide for Single Board Computers (SBCs) such as **Orange Pi (e.g., Orange Pi Zero 3, Orange Pi 3 LTS, Orange Pi One)**, **Raspberry Pi (3 / 4 / 5 / Zero 2W)**, and servers/PCs running **Debian / Ubuntu / Armbian**.

---

## Table of Contents

1. [Hardware Requirements & Wiring](#1-hardware-requirements--wiring)
2. [Base System Packages & Codecs](#2-base-system-packages--codecs)
3. [User Permissions (Serial & Audio Groups)](#3-user-permissions-serial--audio-groups)
4. [Software Installation](#4-software-installation)
   - [Option A: Running Pre-Compiled Binaries from `./build/` (Recommended)](#option-a-running-pre-compiled-binaries-from-build-recommended)
   - [Option B: Compiling from Source Code (Go 1.22+)](#option-b-compiling-from-source-code-go-122)
5. [Hardware Device Identification](#5-hardware-device-identification)
   - [A. Radio Serial CAT Interface (USB-UART)](#a-radio-serial-cat-interface-usb-uart)
   - [B. ALSA Sound Card Devices (Audio RX / TX)](#b-alsa-sound-card-devices-audio-rx--tx)
6. [Application Configuration (`config.json`)](#6-application-configuration-configjson)
7. [APRS Modem Configuration (`direwolf.conf`)](#7-aprs-modem-configuration-direwolfconf)
8. [Automated Systemd Service Setup](#8-automated-systemd-service-setup)
9. [Log Monitoring & Diagnostics](#9-log-monitoring--diagnostics)
10. [OpenWebRX Configuration (Optional)](#10-openwebrx-configuration-optional)
11. [Security, Safety & Operational Best Practices](#11-security-safety--operational-best-practices)

---

## 1. Hardware Requirements & Wiring

1. **Transceiver & Firmware:**
   - **Quansheng UV-K1 / UV-K5 / UV-K6 / UV-5R Plus / UV-K5v3** flashed with the CAT-enabled firmware: **[uv-k1-k5v3-firmware-CAT](https://github.com/sugarfree90/uv-k1-k5v3-firmware-CAT)** by sugarfree90. This firmware provides the essential CAT protocol commands, S-Meter telemetry (`S1`), fast memory scanning (`SCF`), and DTMF packet reporting (`RD...;`).
2. **Hardware Interface (Recommended: AIOC):**
   - **[AIOC (All-In-One-Cable)](https://github.com/skuep/AIOC)** by Simon Kueppers (`skuep`) – a compact, all-in-one USB-C adapter plugging into the radio's Kenwood 2-pin connector. It provides both the **USB-UART serial CAT port** (CDC-ACM `/dev/ttyACM0` at 38400 baud) and the **USB ALSA sound card** (`plughw:1,0` for RX & TX) over a single USB-C cable, eliminating ground loops and messy split wiring.
3. **Alternative Wiring (Traditional Separate Cable Setup):**
   - If not using an AIOC, you can use:
     - **CAT Serial Cable:** Standard USB programming cable (Kenwood 2-pin plug, USB-UART bridge like CH340, CP2102, FTDI).
     - **USB Audio Card:** External USB audio adapter (e.g. C-Media CM108) with 3.5 mm mic input and 3.5 mm line/headphone output.
     - **Audio Connections:**
       - **RX (Reception):** Radio speaker output (SPK) -> USB sound card microphone input (MIC IN).
       - **TX (Transmission):** USB sound card headphone output (HP/LINE OUT) -> Radio microphone input (MIC).

---

## 2. Base System Packages & Codecs

Update your package repositories and install the required multimedia codecs, audio utilities, build tools, and Direwolf modem:

```bash
sudo apt update && sudo apt upgrade -y
sudo apt install -y build-essential curl wget tar openssl \
                    ffmpeg alsa-utils libasound2-dev direwolf
```

---

## 3. User Permissions (Serial & Audio Groups)

By default, non-root Linux users cannot directly access USB serial ports or ALSA sound hardware. Add your user account (e.g., `orangepi` or `pi`) to the `dialout` and `audio` groups:

```bash
sudo usermod -aG dialout,audio $USER
```

> [!IMPORTANT]
> For group changes to take effect, either log out and log back in via SSH, or run:
> ```bash
> newgrp dialout
> newgrp audio
> ```

---

## 4. Software Installation

Create your project working directory and navigate to it:

```bash
mkdir -p ~/catWebservice
cd ~/catWebservice
```

### Option A: Running Pre-Compiled Binaries from `./build/` (Recommended)

Pre-compiled, optimized static binaries for all major architectures are provided in the `build/` directory:

| CPU Architecture | Binary File | Example SBC Hardware |
| :--- | :--- | :--- |
| **ARM64 (64-bit)** | `catWebservice_linux_arm64` | Orange Pi Zero 3, Orange Pi 3 LTS, Raspberry Pi 3/4/5 (64-bit OS) |
| **ARMv7 (32-bit)** | `catWebservice_linux_armv7` | Orange Pi One, Orange Pi PC, Raspberry Pi 2/3 (32-bit OS) |
| **x86_64 (AMD64)** | `catWebservice_linux_amd64` | Standard Linux PCs, Virtual Machines, Cloud VPS |

Check your system architecture:
```bash
uname -m
```

Copy the appropriate binary to your working directory:
```bash
# For 64-bit ARM:
cp build/catWebservice_linux_arm64 ./catWebservice
chmod +x ./catWebservice

# Or for 32-bit ARM:
# cp build/catWebservice_linux_armv7 ./catWebservice
# chmod +x ./catWebservice
```

---

### Option B: Compiling from Source Code (Go 1.22+)

If you prefer compiling directly on your device, install the official Go toolchain:

```bash
# Download official Go package (example for ARM64):
wget https://go.dev/dl/go1.24.4.linux-arm64.tar.gz
sudo rm -rf /usr/local/go
sudo tar -C /usr/local -xzf go1.24.4.linux-arm64.tar.gz
rm go1.24.4.linux-arm64.tar.gz

# Add Go to PATH in ~/.bashrc:
echo 'export PATH=$PATH:/usr/local/go/bin:~/go/bin' >> ~/.bashrc
source ~/.bashrc

# Verify installation:
go version
```

Compile the application:
```bash
cd ~/catWebservice
go mod tidy
go build -o catWebservice
```

---

## 5. Hardware Device Identification

### A. Radio Serial CAT Interface (USB-UART)

Plug the AIOC adapter (or programming cable) into your radio and an available USB port on your SBC. (The AIOC presents itself as a CDC-ACM device, usually `/dev/ttyACM0`). Check the detected device node:
```bash
ls -l /dev/ttyACM* /dev/ttyUSB*
```
Typically, this will be `/dev/ttyACM0` (for AIOC) or `/dev/ttyUSB0` (for CH340/CP2102 programming cables). Note this down for `config.json`.

### B. ALSA Sound Card Devices (Audio RX / TX)

List available recording hardware (RX):
```bash
arecord -l
```
Example output:
```
card 1: Device [USB Audio Device], device 0: USB Audio [USB Audio]
```
Here, the card index is `1` and device index is `0`. In ALSA format, this translates to `"1,0"` or `"plughw:1,0"`.

List available playback hardware (TX):
```bash
aplay -l
```
Identify the corresponding output card number (usually also `"1,0"`).

Set proper audio mixer levels using ALSA mixer:
```bash
alsamixer -c 1
```
*(Use the arrow keys to adjust the MIC recording level to approximately 60–70%, ensure the channel is unmuted [OO], and persist settings with: `sudo alsactl store`)*.

---

## 6. Application Configuration (`config.json`)

Open the configuration file in your preferred text editor:
```bash
nano config.json
```

Adjust the key settings:
1. **`callsign`**: Your amateur radio callsign (e.g., `"N0CALL"`).
2. **`serial_port`**: The radio's serial port path (e.g., `"/dev/ttyACM0"`).
3. **`audio_rx_device`**: ALSA recording card index (e.g., `"1,0"` or `"plughw:1,0"`).
4. **`audio_tx_device`**: ALSA playback card index (e.g., `"1,0"` or `"plughw:1,0"`).
5. **`mqtt_broker`**: URL of your MQTT broker (e.g., `"tcp://127.0.0.1:1883"`).
6. **`log_mode`**: Keep set to `"shm"` to store logs in RAM (`/dev/shm`), protecting your MicroSD card against write exhaustion.

---

## 7. APRS Modem Configuration (`direwolf.conf`)

Edit the Direwolf modem configuration file:
```bash
nano direwolf.conf
```

Set the following parameters:
* **`ADEVICE plughw:1,0 null`** – Specifies the USB audio card as the input device.
* **`MYCALL N0CALL-10`** – Your callsign with an appropriate IGate SSID.
* **`IGLOGIN N0CALL-10 12345`** – Your callsign and official APRS-IS passcode.
* **`PBEACON`** – Your station's latitude, longitude, and beacon comment.

---

## 8. Automated Systemd Service Setup

To run `catWebservice` automatically in the background on system boot with auto-restart on failure, create a systemd unit file:

```bash
sudo nano /etc/systemd/system/catwebservice.service
```

Paste the following unit definition (adjust the `User` and `WorkingDirectory` paths according to your username):

```ini
[Unit]
Description=Quanshengstein CAT Webservice & WebRTC Radio Server
After=network.target sound.target

[Service]
Type=simple
User=orangepi
WorkingDirectory=/home/orangepi/catWebservice
ExecStart=/home/orangepi/catWebservice/catWebservice
Restart=always
RestartSec=3
LimitNOFILE=65535

# Real-time process priority for low-latency ALSA audio
Nice=-10

# Forward stdout to systemd journal
StandardOutput=journal
StandardError=journal

[Install]
WantedBy=multi-user.target
```

Enable and start the service:
```bash
sudo systemctl daemon-reload
sudo systemctl enable catwebservice
sudo systemctl start catwebservice
```

Verify service status:
```bash
sudo systemctl status catwebservice
```

---

## 9. Log Monitoring & Diagnostics

Thanks to the MultiWriter logging subsystem, logs can be monitored through 4 different methods:

1. **Live systemd journal:**
   ```bash
   journalctl -u catwebservice -f
   ```

2. **Direct RAM tmpfs stream (Zero flash disk I/O):**
   ```bash
   tail -n 50 -f /dev/shm/catwebservice.log
   ```

3. **In-Browser Web Console:**
   * Open the web panel: `https://<SERVER-IP>:8443/radio.html`.
   * Click the **`📜 Logs`** button in the top statistics bar next to CPU load.
   * Logs are streamed live directly from RAM memory with syntax highlighting and auto-scroll.

4. **Raw Plain Text Endpoint:**
   * View raw logs in any browser at `https://<SERVER-IP>:8443/logs`.

---

## 10. OpenWebRX Configuration (Optional)

If you run a local **OpenWebRX** SDR receiver on the same device (e.g., on port `8073`):

1. Verify that `owrx_proxy_enabled` is set to `true` in `config.json`.
2. Access the secure OpenWebRX port in your browser:
   ```
   https://<SERVER-IP>:8074/
   ```
3. OpenWebRX will render with the **Quansheng CAT control bar automatically injected** (`owrx.js`).
4. No modifications to OpenWebRX files or templates are needed.
5. Operating in a full HTTPS context ensures the browser grants microphone access for WebRTC PTT transmission.

---

## 11. Security, Safety & Operational Best Practices

### 🔒 Network Security (Remote Access)
* **Never use router DMZ or direct port forwarding:** The application does not contain multi-user password login.
* **Use an encrypted VPN:** For remote operation over the Internet, set up a secure private network such as **Tailscale** (`sudo tailscale up`), **WireGuard**, or **ZeroTier**. This allows encrypted, authenticated access from mobile phones and laptops without opening inbound ports on your home firewall.

### ⚡ Independent Power Cutoff (Hardware Watchdog)
* In unattended or remote installations, an SBC crash or USB lockup could theoretically leave the radio's PTT line activated.
* Connect the radio's 12V power supply to a smart plug (e.g., **Tuya, Shelly, Zigbee, or Tasmota Wi-Fi outlet**). This guarantees that you can remotely cycle DC power to the transceiver in any emergency.

### 📻 Licensing and Operating Regulations
* Transmitting on amateur bands requires a valid amateur radio operator license.
* Ensure your antenna system is properly tuned (acceptable SWR) and adequate grounding is provided.

Vy 73!
