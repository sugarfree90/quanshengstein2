# Podręcznik instalacji i wdrożenia Quanshengstein CAT Webservice

Szczegółowy przewodnik instalacji, konfiguracji i uruchomienia usługi na komputerach jedno-płytkowych (SBC) takich jak **Orange Pi (np. Orange Pi Zero 3, Orange Pi 3 LTS, Orange Pi One)**, **Raspberry Pi (3 / 4 / 5 / Zero 2W)** oraz serwerach i komputerach z systemem **Debian / Ubuntu / Armbian**.

---

## Spis Treści

1. [Wymagania sprzętowe i połączenia](#1-wymagania-sprzętowe-i-połączenia)
2. [Pakiety bazowe i biblioteki](#2-pakiety-bazowe-i-biblioteki)
3. [Uprawnienia użytkownika (dialout i audio)](#3-uprawnienia-użytkownika-dialout-i-audio)
4. [Instalacja oprogramowania](#4-instalacja-oprogramowania)
   - [Opcja A: Uruchomienie gotowej binarki z `./build/` (Zalecana)](#opcja-a-uruchomienie-gotowej-binarki-z-build-zalecana)
   - [Opcja B: Samodzielna kompilacja z kodu źródłowego (Go 1.22+)](#opcja-b-samodzielna-kompilacja-z-kodu-źródłowego-go-122)
5. [Identyfikacja urządzeń sprzętowych](#5-identyfikacja-urządzeń-sprzętowych)
   - [A. Port szeregowy radia (CAT USB)](#a-port-szeregowy-radia-cat-usb)
   - [B. Karta dźwiękowa ALSA (USB Audio)](#b-karta-dźwiękowa-alsa-usb-audio)
6. [Konfiguracja aplikacji (`config.json`)](#6-konfiguracja-aplikacji-configjson)
7. [Konfiguracja modemu APRS (`direwolf.conf`)](#7-konfiguracja-modemu-aprs-direwolfconf)
8. [Autostart usługi w systemd](#8-autostart-usługi-w-systemd)
9. [Podgląd logów i monitorowanie](#9-podgląd-logów-i-monitorowanie)
10. [Konfiguracja OpenWebRX (Opcjonalnie)](#10-konfiguracja-openwebrx-opcjonalnie)

---

## 1. Wymagania sprzętowe i połączenia

1. **Radiotelefon i oprogramowanie:**
   - **Quansheng UV-K1 / UV-K5 / UV-K6 / UV-5R Plus / UV-K5v3** z wgranym oprogramowaniem: **[uv-k1-k5v3-firmware-CAT](https://github.com/sugarfree90/uv-k1-k5v3-firmware-CAT)** autorstwa sugarfree90. To oprogramowanie dostarcza rozszerzenia protokołu CAT, telemetrię S-Metra (`S1`), szybkie sprzętowe skanowanie pamięci (`SCF`) oraz raportowanie odebranych tonów DTMF (`RD...;`).
2. **Interfejs sprzętowy (Rekomendowany: AIOC):**
   - **[AIOC (All-In-One-Cable)](https://github.com/skuep/AIOC)** autorstwa Simona Kueppersa (`skuep`) – miniaturowy adapter USB-C wpinany bezpośrednio w złącze 2-pin Kenwood w radiu. Udostępnia w ramach jednego przewodu USB-C zarówno **port szeregowy USB-UART CAT** (urządzenie CDC-ACM `/dev/ttyACM0`, 38400 baud), jak i **kartę dźwiękową ALSA** (`plughw:1,0` dla RX i TX), eliminując zakłócenia, pętle masy i plątaninę kabli.
3. **Alternatywne połączenie (tradycyjny zestaw rozdzielny):**
   - W przypadku braku płytki AIOC można użyć:
     - **Kabel do programowania / CAT:** Standardowy kabel USB z wtykiem Kenwood 2-pin oparty na konwerterze USB-UART (CH340, CP2102, FTDI).
     - **Karta dźwiękowa USB:** Zewnętrzna karta audio na USB (np. C-Media CM108 z wejściem mikrofonowym 3.5 mm i wyjściem słuchawkowym 3.5 mm).
     - **Połączenia audio:**
       - **RX (Odbiór):** Wyjście słuchawkowe radia (SPK) -> Wejście mikrofonowe karty USB (MIC IN).
       - **TX (Nadawanie):** Wyjście słuchawkowe karty USB (LINE/HP OUT) -> Wejście mikrofonowe radia (MIC).

---

## 2. Pakiety bazowe i biblioteki

Zaktualizuj repozytoria i zainstaluj pakiety niezbędne do obsługi dźwięku, kompilacji oraz modemu APRS:

```bash
sudo apt update && sudo apt upgrade -y
sudo apt install -y build-essential curl wget tar openssl \
                    ffmpeg alsa-utils libasound2-dev direwolf
```

---

## 3. Uprawnienia użytkownika (dialout i audio)

Domyślnie zwykły użytkownik Linuksa nie ma bezpośredniego dostępu do portów szeregowych USB ani urządzeń dźwiękowych ALSA. Dodaj swojego użytkownika (np. `orangepi` lub `pi`) do grup `dialout` i `audio`:

```bash
sudo usermod -aG dialout,audio $USER
```

> [!IMPORTANT]
> Aby zmiany odniosły skutek, wyloguj się i zaloguj ponownie przez SSH, albo wykonaj:
> ```bash
> newgrp dialout
> newgrp audio
> ```

---

## 4. Instalacja oprogramowania

Utwórz katalog roboczy i pobierz pliki projektu:

```bash
mkdir -p ~/catWebservice
cd ~/catWebservice
```

### Opcja A: Uruchomienie gotowej binarki z `./build/` (Zalecana)

W katalogu `build/` znajdują się prekompilowane, zoptymalizowane pliki binarne dla popularnych architektur:

| Architektura procesora | Plik binarny | Przykładowe urządzenia |
| :--- | :--- | :--- |
| **ARM64 (64-bit)** | `catWebservice_linux_arm64` | Orange Pi Zero 3, Orange Pi 3 LTS, Raspberry Pi 3/4/5 (64-bit OS) |
| **ARMv7 (32-bit)** | `catWebservice_linux_armv7` | Orange Pi One, Orange Pi PC, Raspberry Pi 2/3 (32-bit OS) |
| **x86_64 (AMD64)** | `catWebservice_linux_amd64` | Standardowe serwery PC, laptopy, maszyny wirtualne x64 |

Sprawdź architekturę swojego urządzenia:
```bash
uname -m
```

Skopiuj odpowiedni plik:
```bash
# Dla ARM64:
cp build/catWebservice_linux_arm64 ./catWebservice
chmod +x ./catWebservice

# Lub dla ARMv7 (32-bit):
# cp build/catWebservice_linux_armv7 ./catWebservice
# chmod +x ./catWebservice
```

---

### Opcja B: Samodzielna kompilacja z kodu źródłowego (Go 1.22+)

Jeśli wolisz skompilować binarkę na miejscu, zainstaluj oficjalne środowisko Go:

```bash
# Pobranie oficjalnego pakietu Go (przykład dla ARM64):
wget https://go.dev/dl/go1.24.4.linux-arm64.tar.gz
sudo rm -rf /usr/local/go
sudo tar -C /usr/local -xzf go1.24.4.linux-arm64.tar.gz
rm go1.24.4.linux-arm64.tar.gz

# Dodanie Go do PATH:
echo 'export PATH=$PATH:/usr/local/go/bin:~/go/bin' >> ~/.bashrc
source ~/.bashrc

# Weryfikacja:
go version
```

Skompiluj aplikację:
```bash
cd ~/catWebservice
go mod tidy
go build -o catWebservice
```

---

## 5. Identyfikacja urządzeń sprzętowych

### A. Port szeregowy radia (CAT USB)

Podłącz adapter AIOC (lub tradycyjny kabel do programowania) do radia i wolnego portu USB w komputerze/SBC. (Adapter AIOC zgłasza się jako urządzenie CDC-ACM, zazwyczaj `/dev/ttyACM0`). Sprawdź wykryty port szeregowy:
```bash
ls -l /dev/ttyACM* /dev/ttyUSB*
```
Zazwyczaj będzie to `/dev/ttyACM0` (dla AIOC) lub `/dev/ttyUSB0` (dla tradycyjnych kabli z układami CH340/CP2102). Zapisz tę ścieżkę do pliku `config.json`.

### B. Karta dźwiękowa ALSA (USB Audio)

Wyświetl listę kart nagrywających (RX):
```bash
arecord -l
```
Przykładowy wynik:
```
card 1: Device [USB Audio Device], device 0: USB Audio [USB Audio]
```
Numer karty to `1`, numer urządzenia to `0`. Odpowiada to oznaczeniu `"1,0"` lub `"plughw:1,0"`.

Wyświetl listę kart odtwarzających (TX):
```bash
aplay -l
```
Zanotuj numer karty wyjściowej (np. również `"1,0"`).

Ustaw właściwe poziomy wzmocnienia miksera ALSA:
```bash
alsamixer -c 1
```
*(Klawiszami strzałek ustaw poziom MIC na około 60-70%, upewnij się, że nie ma wyciszenia [MM], zapisz stan poleceniem: `sudo alsactl store`)*.

---

## 6. Konfiguracja aplikacji (`config.json`)

Otwórz plik konfiguracyjny w edytorze:
```bash
nano config.json
```

Dostosuj kluczowe wartości:
1. **`callsign`**: Twój znak krótkofalarski (np. `"SQ3XYZ"`).
2. **`serial_port`**: Ścieżka do portu CAT (np. `"/dev/ttyACM0"`).
3. **`audio_rx_device`**: Karta wejściowa ALSA (np. `"1,0"` lub `"plughw:1,0"`).
4. **`audio_tx_device`**: Karta wyjściowa ALSA (np. `"1,0"` lub `"plughw:1,0"`).
5. **`mqtt_broker`**: Adres Twojego brokera MQTT (np. `"tcp://192.168.1.50:1883"`).
6. **`log_mode`**: Pozostaw `"shm"` (zapis do pamięci RAM `/dev/shm`), co chroni kartę MicroSD przed zużyciem.

---

## 7. Konfiguracja modemu APRS (`direwolf.conf`)

Edytuj plik konfiguracji Direwolfa:
```bash
nano direwolf.conf
```

Uzupełnij:
* **`ADEVICE plughw:1,0 null`** – karta USB radia jako wejście dźwięku.
* **`MYCALL SQ3XYZ-10`** – Twój znak z SSID dla stacji IGate.
* **`IGLOGIN SQ3XYZ-10 12345`** – Twój znak i kod passcode APRS-IS.
* **`PBEACON`** – współrzędne geograficzne Twojej stacji.

---

## 8. Autostart usługi w systemd

Aby webservice startował automatycznie przy uruchomieniu systemu i działał w tle z pełnym restartem po awarii, utwórz plik jednostki systemd:

```bash
sudo nano /etc/systemd/system/catwebservice.service
```

Wklej poniższą zawartość (dostosuj ścieżkę i nazwę użytkownika, np. `orangepi`):

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

# Uprawnienia czasu rzeczywistego dla ALSA
Nice=-10

# Przekazywanie logów do journald
StandardOutput=journal
StandardError=journal

[Install]
WantedBy=multi-user.target
```

Włącz i uruchom usługę:
```bash
sudo systemctl daemon-reload
sudo systemctl enable catwebservice
sudo systemctl start catwebservice
```

Sprawdź status usługi:
```bash
sudo systemctl status catwebservice
```

---

## 9. Podgląd logów i monitorowanie

Dzięki wielokanałowemu systemowi logowania (MultiWriter) masz dostęp do logów na 4 sposoby:

1. **Podgląd na żywo w systemd:**
   ```bash
   journalctl -u catwebservice -f
   ```

2. **Podgląd z pliku w pamięci RAM (`tmpfs` – brak zapisu na dysk):**
   ```bash
   tail -n 50 -f /dev/shm/catwebservice.log
   ```

3. **Konsola Live w przeglądarce:**
   * Otwórz panel WWW: `https://<IP-SERWERA>:8443/radio.html`.
   * Kliknij przycisk **`📜 Logs`** w górnym pasku statystyk obok zużycia procesora.
   * Logi ładują się prosto z pamięci RAM z kolorowaniem składni i funkcją Auto-scroll.

4. **Widok surowy (Plain Text):**
   * Bezpośredni podgląd w przeglądarce pod adresem `https://<IP-SERWERA>:8443/logs`.

---

## 10. Konfiguracja OpenWebRX (Opcjonalnie)

Jeśli na urządzeniu masz uruchomiony lokalny odbiornik SDR **OpenWebRX** (np. na porcie `8073`):

1. Upewnij się, że w `config.json` opcja `owrx_proxy_enabled` ma wartość `true`.
2. Otwórz w przeglądarce dedykowany bezpieczny port OpenWebRX:
   ```
   https://<IP-SERWERA>:8074/
   ```
3. Odbiornik OpenWebRX otworzy się z **automatycznie wstrzykniętym panelem sterowania CAT** (`owrx.js`).
4. Nie musisz modyfikować żadnych plików OpenWebRX ani dodawać skryptów w opisie stacji.
5. Połączenie działa w pełnym kontekście HTTPS, zapewniając prawidłowy dostęp do mikrofonu przeglądarki podczas nadawania PTT.

---

## 11. Bezpieczeństwo i Dobre Praktyki Eksploatacji

### 🔒 Bezpieczeństwo sieciowe (Dostęp zdalny)
* **Nie przekierowuj portów na routerze (DMZ / Port Forwarding):** Aplikacja nie posiada wbudowanego uwierzytelniania hasłem ani logowania użytkowników.
* **Użyj VPN:** Do bezpiecznego łączenia się z radiem przez Internet skonfiguruj bezpłatny tunel **Tailscale** (`sudo tailscale up`), **WireGuard** lub **ZeroTier**. Pozwoli to na szyfrowany dostęp do radia z telefonu i laptopa z dowolnego miejsca na świecie bez otwierania portów w firewallu routera.

### ⚡ Niezależne odcięcie zasilania radia (Hard Power Cutoff)
* W instalacjach bezobsługowych (remote base / repeater) komputer SBC może ulec zawieszeniu lub awarii magistrali USB w momencie, gdy radio miało aktywną linię PTT.
* Podłącz zasilacz radia do zdalnie sterowanego gniazdka (np. **gniazdko Wi-Fi Tuya / Tasmota / Shelly / Zigbee**). Umożliwi to twardy restart zasilania radia w razie jakiejkolwiek awarii.

### 📻 Prawo i uprawnienia
* Nadawanie w pasmach amatorskich wymaga ważnego pozwolenia radiowego (licencji krótkofalarskiej).
* Zadbaj o prawidłowe dopasowanie anteny (SWR) i uziemienie instalacji.

Vy 73!

