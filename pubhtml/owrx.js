const WS_PORT = 8081;

// Auto-detect backend host and protocol (supports embedding into OpenWebRX from different port/domain)
const _backendOrigin = (function() {
    if (window.CAT_BACKEND_URL) {
        try {
            const u = new URL(window.CAT_BACKEND_URL, window.location.href);
            return {
                host: u.host,
                protocol: u.protocol,
                wsProto: u.protocol === 'https:' ? 'wss://' : 'ws://'
            };
        } catch(e) {}
    }

    let scriptSrc = "";
    if (document.currentScript && document.currentScript.src) {
        scriptSrc = document.currentScript.src;
    } else {
        const scripts = document.getElementsByTagName('script');
        for (let i = scripts.length - 1; i >= 0; i--) {
            let s = scripts[i];
            if (s.src && s.src.includes('owrx.js')) {
                scriptSrc = s.src;
                break;
            }
        }
    }

    if (scriptSrc) {
        try {
            const u = new URL(scriptSrc, window.location.href);
            return {
                host: u.host,
                protocol: u.protocol,
                wsProto: u.protocol === 'https:' ? 'wss://' : 'ws://'
            };
        } catch(e) {}
    }

    return {
        host: window.location.host,
        protocol: window.location.protocol,
        wsProto: window.location.protocol === 'https:' ? 'wss://' : 'ws://'
    };
})();

let txSocket = null;
let isTransmitting = false;
let isConnected = false;

let audioContext;
let micStream;
let micReady = false;
let micInitInProgress = false; 

let lastProfileName = "";
let lastPolledFreq = 0;
window.preTxVolume = null;
window.isStickyPtt = false;

let rxAudioEnabled = false;

// --- OPUS WEB CODECS API (KEPT FOR TRANSMISSION) ---
let opusEncoder = null;
let txTimestamp = 0;
let txPcmBuffer = new Float32Array(0);

// --- WEBRTC (Audio receive system) ---
let webrtcPC = null;
let remoteAudioEl = new Audio();
remoteAudioEl.autoplay = true;
remoteAudioEl.muted = true; // Muted by default so Web Audio API GainNode handles playback without double-audio
window.remoteAudioEl = remoteAudioEl;

// --- AUDIO WORKLET (TxProcessor for clear transmit) ---
const workletCode = `
    class TxProcessor extends AudioWorkletProcessor {
        process(inputs, outputs) {
            const input = inputs[0];
            if (input && input.length > 0 && input[0]) {
                this.port.postMessage(new Float32Array(input[0]));
            }
            const output = outputs[0];
            if (output && output.length > 0 && output[0]) {
                output[0].fill(0);
            }
            return true;
        }
    }
    registerProcessor('tx-processor', TxProcessor);
`;

let workletsLoaded = false;
async function loadAudioWorklets() {
    if (workletsLoaded) return;
    const blob = new Blob([workletCode], { type: 'application/javascript' });
    const url = URL.createObjectURL(blob);
    await audioContext.audioWorklet.addModule(url);
    workletsLoaded = true;
}

// --- SCANNER VARIABLES ---
window.sqlOpen = false;
window.isScanning = false;
let scanTimer = null;
let scanDelay = 1200; 

let wakeLock = null;
async function requestWakeLock() {
    if ('wakeLock' in navigator) {
        try { wakeLock = await navigator.wakeLock.request('screen'); } catch (err) {}
    }
}
document.addEventListener('visibilitychange', async () => { if (wakeLock !== null && document.visibilityState === 'visible') requestWakeLock(); });

function updateConnectionState(state) {
    isConnected = state;

    let btn = document.getElementById('ptt-button');
    let btnTxt = document.getElementById('ptt-text');
    let container = document.getElementById('ptt-container');
    
    if (!btn || !btnTxt || !container) return;

    if (!isConnected) {
        container.style.backgroundColor = "#555";
        btn.style.cursor = "not-allowed";
        btnTxt.innerText = "No connection to radio (Offline)";
        
        if(window.isStickyPtt) {
            window.isStickyPtt = false;
            let stickyBtn = document.getElementById('ptt-sticky');
            if(stickyBtn) { stickyBtn.style.backgroundColor = "rgba(0,0,0,0.2)"; stickyBtn.innerHTML = "🔓"; }
        }
    } else {
        if (isTransmitting) {
            container.style.backgroundColor = "red";
            btnTxt.innerText = "TRANSMITTING (TX)";
        } else if (micReady) {
            container.style.backgroundColor = "#4CAF50";
            btn.style.cursor = "pointer";
            btnTxt.innerText = "Ready! Push PTT";
        } else {
            container.style.backgroundColor = "#2196F3";
            btn.style.cursor = "pointer";
            btnTxt.innerText = "🎙️ Click to activate microphone";
        }
    }
}

async function initRxPlayer() {
    if (!audioContext) audioContext = new (window.AudioContext || window.webkitAudioContext)({ sampleRate: 48000 });
    await loadAudioWorklets();
}

window.toggleScan = function(state) {
    if (typeof window.toggleScanTab === 'function') {
        if (state !== undefined) {
            if (state) {
                if (typeof window.startScan === 'function') window.startScan("tab");
                else window.toggleScanTab();
            } else {
                if (typeof window.stopScan === 'function') window.stopScan();
            }
        } else {
            window.toggleScanTab();
        }
        return;
    }
    window.isScanning = state;
    let label = document.getElementById('scan-label');
    if (state) {
        if (label) label.style.backgroundColor = "#2196F3";
        if (txSocket && txSocket.readyState === WebSocket.OPEN) {
            txSocket.send(JSON.stringify({ cmd: "start_scan" }));
        } else {
            scanNext();
        }
    } else {
        if (label) label.style.backgroundColor = "#444";
        clearTimeout(scanTimer);
        if (typeof window.clearScanHighlights === 'function') {
            window.clearScanHighlights();
        }
        if (typeof window.updateAudioMute === 'function') {
            window.updateAudioMute();
        } else if (typeof remoteAudioEl !== 'undefined' && remoteAudioEl) {
            if (!rxSourceNode) {
                remoteAudioEl.muted = false;
                if (remoteAudioEl.paused) remoteAudioEl.play().catch(()=>{});
            } else {
                remoteAudioEl.muted = true;
            }
        }
        if (txSocket && txSocket.readyState === WebSocket.OPEN) {
            txSocket.send(JSON.stringify({ cmd: "stop_scan" }));
        }
    }
};

function scanNext() {
    if (!window.isScanning) return;
    if (window.sqlOpen) {
        scanTimer = setTimeout(scanNext, 1000); 
        return;
    }
    let select = document.getElementById("openwebrx-sdr-profiles-listbox");
    if (select && select.options.length > 0) {
        select.selectedIndex = (select.selectedIndex + 1) % select.options.length;
        window.applyProfileSettings(); 
    }
    scanTimer = setTimeout(scanNext, scanDelay);
}

window.toggleMon = function(state) {
    if (!isConnected || txSocket.readyState !== WebSocket.OPEN) return;
    txSocket.send(JSON.stringify({ cmd: "set_monitor", val: state ? 1 : 0 }));
};

window.syncRadioProfile = function() {
    if (!isConnected || txSocket.readyState !== WebSocket.OPEN) return;
    let profile = getActiveProfile();
    if (!profile || profile.freq === 0) return; 

    let pwrSelect = document.getElementById('tx-power');
    let ctcssSelect = document.getElementById('tx-ctcss');
    let dcsSelect = document.getElementById('tx-dcs');
    let shiftDirSelect = document.getElementById('tx-off-dir');
    let shiftValInput = document.getElementById('tx-off-val');
    
    txSocket.send(JSON.stringify({
        cmd: "apply_profile",
        freq: profile.freq,
        mod: profile.mod,
        pwr: pwrSelect ? parseInt(pwrSelect.value) : 7,
        ctcss: ctcssSelect ? parseFloat(ctcssSelect.value) : 0,
        dcs: dcsSelect ? parseInt(dcsSelect.value) : 0,
        shift_dir: shiftDirSelect ? parseInt(shiftDirSelect.value) : 0,
        shift_val: shiftValInput ? parseFloat(shiftValInput.value) : 0.0
    }));
};

// --- COOKIE HELPERS ---
function setCookie(name, value, days = 365) {
    let expires = "";
    if (days) {
        let date = new Date();
        date.setTime(date.getTime() + (days * 24 * 60 * 60 * 1000));
        expires = "; expires=" + date.toUTCString();
    }
    document.cookie = name + "=" + encodeURIComponent(value) + expires + "; path=/; SameSite=Lax";
}

function getCookie(name) {
    let nameEQ = name + "=";
    let ca = document.cookie.split(';');
    for (let i = 0; i < ca.length; i++) {
        let c = ca[i];
        while (c.charAt(0) === ' ') c = c.substring(1, c.length);
        if (c.indexOf(nameEQ) === 0) return decodeURIComponent(c.substring(nameEQ.length, c.length));
    }
    return null;
}

// --- RECEIVE GAIN (RX GAIN) ---
let rxStream = null;
let rxGainNode = null;
let rxSourceNode = null;
let rxMuted = false;

function getSavedRxGain() {
    let val = getCookie('rx_audio_gain');
    if (val === null || val === "") {
        val = localStorage.getItem('rx_audio_gain');
    }
    if (val === null || val === "" || isNaN(parseFloat(val))) {
        return "1.0";
    }
    return val;
}

let rxCurrentGain = parseFloat(getSavedRxGain());

window.updateRxGain = function(newVal) {
    let slider = document.getElementById('rx-gain-slider');
    let valDisp = document.getElementById('rx-gain-val');
    if (slider) {
        if (newVal !== undefined) {
            slider.value = newVal;
        }
        let val = parseFloat(slider.value);
        if (isNaN(val) || val < 0) val = 1.0;
        if (valDisp) valDisp.innerText = val.toFixed(1) + "x";
        
        rxCurrentGain = val;
        setCookie('rx_audio_gain', val.toString(), 365);
        try { localStorage.setItem('rx_audio_gain', val.toString()); } catch(e) {}

        window.applyRxGain();
    }
};

window.applyRxGain = function() {
    if (rxGainNode && audioContext) {
        let target = rxMuted ? 0 : rxCurrentGain;
        rxGainNode.gain.setTargetAtTime(target, audioContext.currentTime, 0.03);
    }
    if (remoteAudioEl) {
        if (!rxSourceNode) {
            remoteAudioEl.muted = rxMuted;
            remoteAudioEl.volume = Math.min(1.0, rxCurrentGain);
        } else {
            // Strictly enforce muted state on the HTML5 audio element
            // to prevent duplicate audio / echo when rxGainNode is active
            remoteAudioEl.muted = true;
        }
    }
};

window.setRxMuted = function(muted) {
    rxMuted = muted;
    window.applyRxGain();
};

function setupRxAudioPipeline(stream) {
    rxStream = stream;

    if (!audioContext) {
        audioContext = new (window.AudioContext || window.webkitAudioContext)({ sampleRate: 48000 });
    }
    if (audioContext.state === 'suspended') {
        audioContext.resume().catch(()=>{});
    }

    // Attach stream to remoteAudioEl (keeps track alive in WebRTC),
    // but mute remoteAudioEl because sound is routed through rxGainNode.
    remoteAudioEl.srcObject = stream;
    remoteAudioEl.muted = true;
    remoteAudioEl.play().catch(e => console.error("Autoplay blocked. Click interface to activate!", e));

    try {
        if (rxSourceNode) {
            try { rxSourceNode.disconnect(); } catch(e) {}
        }
        rxSourceNode = audioContext.createMediaStreamSource(stream);
        window.rxSourceNode = rxSourceNode;
        if (!rxGainNode) {
            rxGainNode = audioContext.createGain();
            window.rxGainNode = rxGainNode;
            rxGainNode.connect(audioContext.destination);
        }
        rxSourceNode.connect(rxGainNode);
        remoteAudioEl.muted = true;
        window.applyRxGain();
    } catch(err) {
        console.warn("[WebRTC] createMediaStreamSource error, fallback to remoteAudioEl:", err);
        rxSourceNode = null;
        window.rxSourceNode = null;
        remoteAudioEl.muted = rxMuted;
        remoteAudioEl.volume = Math.min(1.0, rxCurrentGain);
    }
}

async function startWebRTCConnection() {
    if (!rxAudioEnabled) return; // Guard against double execution
    
    console.log("[WebRTC] Server confirmed audio system ready! Connecting...");
    let rxLabel = document.getElementById('rx-audio-label');
    if (rxLabel) {
        rxLabel.style.backgroundColor = "#4CAF50";
        rxLabel.querySelector('span').innerText = "Radio RX";
    }
    
    if (webrtcPC) webrtcPC.close();
    webrtcPC = new RTCPeerConnection();
    
    webrtcPC.ontrack = (event) => {
        console.log("[WebRTC] Received audio stream. Playing...");
        let stream = (event.streams && event.streams[0]) ? event.streams[0] : new MediaStream([event.track]);
        setupRxAudioPipeline(stream);
    };
    
    webrtcPC.addTransceiver('audio', { direction: 'recvonly' });
    
    const offer = await webrtcPC.createOffer();
    await webrtcPC.setLocalDescription(offer);
    
    try {
        let host = _backendOrigin.host; 
        let protocol = _backendOrigin.protocol;
        
        const response = await fetch(`${protocol}//${host}/webrtc-api/radio/whep`, {
            method: 'POST',
            body: webrtcPC.localDescription.sdp,
            headers: { 'Content-Type': 'application/sdp' }
        });
        
        if (!response.ok) {
            const errText = await response.text();
            throw new Error(`Code ${response.status}: ${errText}`);
        }

        const answerSdp = await response.text();
        await webrtcPC.setRemoteDescription(new RTCSessionDescription({ type: 'answer', sdp: answerSdp }));
        console.log("[WebRTC] Connected to audio stream successfully!");
    } catch (err) {
        console.error("[WebRTC] Audio stream not ready yet (ALSA FFmpeg delay). Retrying...", err);
        
        // Revert button to orange waiting mode
        let rxLabel = document.getElementById('rx-audio-label');
        if (rxLabel) {
            rxLabel.style.backgroundColor = "#ff9800";
            rxLabel.querySelector('span').innerText = "Listening...";
        }
        
        setTimeout(() => {
            if (rxAudioEnabled && txSocket && txSocket.readyState === WebSocket.OPEN) {
                txSocket.send(JSON.stringify({ cmd: "check_audio_system" }));
            }
        }, 2000);
    }
}

window.toggleRxAudio = async function(state) {
    console.log(`[WebRTC] Audio state request: ${state}`);
    rxAudioEnabled = state;
    localStorage.setItem('tx_rx_audio', state ? "1" : "0");
    setCookie('tx_rx_audio', state ? "1" : "0", 365);
    
    let rxLabel = document.getElementById('rx-audio-label');

    if (state) {
        if (!audioContext) audioContext = new (window.AudioContext || window.webkitAudioContext)({ sampleRate: 48000 });
        if (audioContext && audioContext.state === 'suspended') await audioContext.resume().catch(()=>{});

        if(rxLabel) {
            rxLabel.style.backgroundColor = "#ff9800"; // Orange waiting mode
            rxLabel.querySelector('span').innerText = "Listening...";
        }
        
        if (txSocket && txSocket.readyState === WebSocket.OPEN) {
            txSocket.send(JSON.stringify({ cmd: "check_audio_system" }));
        }
    } else {
        if(rxLabel) {
            rxLabel.style.backgroundColor = "#444";
            rxLabel.style.color = "#fff";
            rxLabel.style.textShadow = "none";
            rxLabel.style.boxShadow = "none";
            rxLabel.querySelector('span').innerText = "Radio RX";
        }
        if (webrtcPC) {
            webrtcPC.close();
            webrtcPC = null;
        }
        if (rxSourceNode) {
            try { rxSourceNode.disconnect(); } catch(e) {}
            rxSourceNode = null;
        }
        remoteAudioEl.pause();
        remoteAudioEl.srcObject = null;
    }

    if (typeof window.updateAudioMute === 'function') {
        window.updateAudioMute();
    } else {
        window.setRxMuted(isTransmitting);
    }
};

window.toggleAdvancedSettings = function() {
    let advPanel = document.getElementById('tx-advanced-settings');
    if (advPanel.style.display === 'none') {
        advPanel.style.display = 'flex';
        populateMics();
    } else {
        advPanel.style.display = 'none';
    }
};

window.micAnalyser = null;
window.drawSpectrum = function() {
    if (!window.micAnalyser) return;
    requestAnimationFrame(window.drawSpectrum);

    let canvas = document.getElementById('ptt-spectrum');
    if (!canvas) return;
    let ctx = canvas.getContext('2d');

    if (canvas.width !== canvas.offsetWidth) canvas.width = canvas.offsetWidth;
    if (canvas.height !== canvas.offsetHeight) canvas.height = canvas.offsetHeight;

    let bufferLength = window.micAnalyser.frequencyBinCount; 
    let dataArray = new Uint8Array(bufferLength);
    window.micAnalyser.getByteFrequencyData(dataArray); 

    ctx.clearRect(0, 0, canvas.width, canvas.height);
    let barWidth = (canvas.width / bufferLength) * 2.5;
    let barHeight;
    let x = 0;

    for(let i = 0; i < bufferLength; i++) {
        barHeight = (dataArray[i] / 255) * canvas.height;
        ctx.fillStyle = 'rgba(255, 255, 255, 0.4)'; 
        ctx.fillRect(x, canvas.height - barHeight, barWidth, barHeight);
        x += barWidth + 1;
    }
};

async function populateMics() {
    try {
        let devices = await navigator.mediaDevices.enumerateDevices();
        let mics = devices.filter(d => d.kind === 'audioinput');
        let select = document.getElementById('mic-select');
        if (!select) return;
        
        let activeDeviceId = null;
        if (micStream && micStream.getAudioTracks().length > 0) activeDeviceId = micStream.getAudioTracks()[0].getSettings().deviceId;

        select.innerHTML = mics.map(m => `<option value="${m.deviceId}">${m.label || 'Microphone ' + m.deviceId.substring(0,5)}</option>`).join('');
        
        if (activeDeviceId && Array.from(select.options).some(o => o.value === activeDeviceId)) select.value = activeDeviceId;
        else if (select.value && Array.from(select.options).some(o => o.value === select.value)) select.value = select.value;
    } catch(e) {}
}

window.changeMicrophone = async function() {
    if (micReady) {
        if (micStream) micStream.getTracks().forEach(track => track.stop());
        let constraints = { audio: { echoCancellation: false, noiseSuppression: false, autoGainControl: false, channelCount: 1 } };
        let ms = document.getElementById('mic-select');
        if (ms && ms.value) constraints.audio.deviceId = { exact: ms.value };
        
        try {
            micStream = await navigator.mediaDevices.getUserMedia(constraints);
            if (window.micSourceNode) window.micSourceNode.disconnect();
            window.micSourceNode = audioContext.createMediaStreamSource(micStream);
            window.updateMicRouting();
        } catch (err) { console.error("Microphone error:", err); }
    }
};

window.micGainNode = null;
window.micCompressorNode = null;
window.micSourceNode = null;

window.refreshMics = async function(btn) {
    let oldText = btn.innerText;
    btn.innerText = "...";
    await populateMics();
    btn.innerText = oldText;
}

window.updateMicEffects = function() {
    let gainSlider = document.getElementById('mic-gain-slider');
    let gainValDisp = document.getElementById('mic-gain-val');
    if (gainSlider && gainValDisp) {
        let val = parseFloat(gainSlider.value);
        gainValDisp.innerText = val.toFixed(1) + "x";
        localStorage.setItem('tx_mic_gain', val);
        setCookie('tx_mic_gain', val.toString(), 365);
        if (window.micGainNode && audioContext) {
            window.micGainNode.gain.setTargetAtTime(val, audioContext.currentTime, 0.05);
        }
    }
};

window.updateMicRouting = function() {
    let compEnable = document.getElementById('mic-comp-enable');
    if (compEnable) {
        let val = compEnable.checked ? "1" : "0";
        localStorage.setItem('tx_mic_comp', val);
        setCookie('tx_mic_comp', val, 365);
    }

    if (!window.micSourceNode || !window.micGainNode || !window.micCompressorNode || !window.micAnalyser || !window.txWorkletNode) return;
    
    try { window.micSourceNode.disconnect(); } catch(e) {}
    try { window.micGainNode.disconnect(); } catch(e) {}
    try { window.micCompressorNode.disconnect(); } catch(e) {}
    try { window.micAnalyser.disconnect(); } catch(e) {}
    
    window.micSourceNode.connect(window.micGainNode);
    
    if (compEnable && compEnable.checked) {
        window.micGainNode.connect(window.micCompressorNode);
        window.micCompressorNode.connect(window.micAnalyser);
    } else {
        window.micGainNode.connect(window.micAnalyser);
    }
    window.micAnalyser.connect(window.txWorkletNode);
};

function createTxPanel() {
    if (document.getElementById('tx-panel')) document.getElementById('tx-panel').remove();

    let panel = document.createElement('div');
    panel.id = 'tx-panel';
    Object.assign(panel.style, { 
        position: 'fixed', bottom: '90px', left: '50%', transform: 'translateX(-50%)', 
        backgroundColor: 'rgba(30, 30, 30, 0.95)', color: '#fff', padding: '10px 15px', 
        borderRadius: '12px', display: 'flex', flexDirection: 'column', 
        width: '95%', maxWidth: '450px', zIndex: '9998', 
        fontFamily: 'Arial, sans-serif', fontSize: '14px', border: '1px solid #555', 
        boxShadow: '0 4px 15px rgba(0,0,0,0.5)', userSelect: 'none', 
        boxSizing: 'border-box' 
    });

    const ctcssTones = [0, 67.0, 69.3, 71.9, 74.4, 77.0, 79.7, 82.5, 85.4, 88.5, 91.5, 94.8, 97.4, 100.0, 103.5, 107.2, 110.9, 114.8, 118.8, 123.0, 127.3, 131.8, 136.5, 141.3, 146.2, 151.4, 156.7, 162.2, 167.9, 173.8, 179.9, 186.2, 192.8, 203.5, 210.7, 218.1, 225.7, 233.6, 241.8, 250.3];
    const dcsCodes = [0, 23, 25, 26, 31, 32, 43, 47, 51, 54, 65, 71, 72, 73, 74, 114, 115, 116, 125, 131, 132, 134, 143, 152, 155, 156, 162, 165, 172, 174, 205, 223, 226, 243, 244, 245, 251, 261, 263, 265, 271, 306, 311, 315, 331, 343, 346, 351, 364, 365, 371, 411, 412, 413, 423, 431, 432, 445, 464, 465, 466, 503, 506, 516, 532, 546, 565, 606, 612, 624, 627, 631, 632, 654, 662, 664, 703, 712, 723, 731, 732, 734, 743, 754];

    let ctcssOptions = ctcssTones.map(t => `<option value="${t}">${t === 0 ? 'OFF' : t.toFixed(1) + ' Hz'}</option>`).join('');
    let dcsOptions = dcsCodes.map(c => `<option value="${c}">${c === 0 ? 'OFF' : 'DCS ' + String(c).padStart(3, '0')}</option>`).join('');

    let savedGain = getCookie('tx_mic_gain') || localStorage.getItem('tx_mic_gain') || "1.0";
    let savedComp = (getCookie('tx_mic_comp') || localStorage.getItem('tx_mic_comp')) === "1" ? "checked" : "";
    let savedRxAudio = (getCookie('tx_rx_audio') || localStorage.getItem('tx_rx_audio')) === "1" ? "checked" : "";
    let savedRxGain = getSavedRxGain();

    panel.innerHTML = `
        <div style="display:flex; justify-content:space-between; align-items:center; width:100%; gap:8px;">
            <button onclick="window.toggleAdvancedSettings()" style="background:#444; color:#fff; border:1px solid #666; padding:8px 12px; border-radius:5px; cursor:pointer; font-size:16px; flex-shrink:0;" title="Audio & Radio Settings">⚙️</button>
            
            <!-- COMPACT S-METER -->
            <div id="compact-smeter-container" style="display:flex; align-items:center; justify-content:center; gap:6px; background:rgba(0,0,0,0.6); border:1px solid #444; border-radius:6px; padding:4px 8px; flex:1; min-width:0; overflow:hidden;" title="Signal Strength (S-Meter)">
                <span id="compact-smeter-val" style="font-family:monospace; font-size:13px; font-weight:bold; color:#888; min-width:32px; text-align:center; white-space:nowrap;">S0</span>
                <div id="compact-smeter-bars" style="display:flex; gap:2px; align-items:flex-end; height:16px;">
                    <span id="compact-sbar-1" style="display:inline-block; width:3px; height:8px; background:#333; border-radius:1px;"></span>
                    <span id="compact-sbar-2" style="display:inline-block; width:3px; height:9px; background:#333; border-radius:1px;"></span>
                    <span id="compact-sbar-3" style="display:inline-block; width:3px; height:10px; background:#333; border-radius:1px;"></span>
                    <span id="compact-sbar-4" style="display:inline-block; width:3px; height:11px; background:#333; border-radius:1px;"></span>
                    <span id="compact-sbar-5" style="display:inline-block; width:3px; height:12px; background:#333; border-radius:1px;"></span>
                    <span id="compact-sbar-6" style="display:inline-block; width:3px; height:13px; background:#333; border-radius:1px;"></span>
                    <span id="compact-sbar-7" style="display:inline-block; width:3px; height:14px; background:#333; border-radius:1px;"></span>
                    <span id="compact-sbar-8" style="display:inline-block; width:3px; height:15px; background:#333; border-radius:1px;"></span>
                    <span id="compact-sbar-9" style="display:inline-block; width:3px; height:16px; background:#333; border-radius:1px;"></span>
                </div>
            </div>

            <div style="display:flex; gap:8px; align-items:center; flex-shrink:0;">
                <label id="rx-audio-label" style="display:flex; align-items:center; gap:5px; cursor:pointer; background:#444; padding:8px 12px; border-radius:5px; font-weight:bold;">
                    <input type="checkbox" id="rx-audio-toggle" onchange="window.toggleRxAudio(this.checked)" style="transform: scale(1.2);" ${savedRxAudio}>
                    <span>Radio RX</span>
                </label>
                <label style="display:flex; align-items:center; gap:5px; cursor:pointer; background:#8b0000; padding:8px 12px; border-radius:5px; font-weight:bold;">
                    <input type="checkbox" id="mon-toggle" onchange="window.toggleMon(this.checked);" style="transform: scale(1.2);">
                    MON
                </label>
            </div>
        </div>

        <div id="tx-advanced-settings" style="display:none; flex-direction:column; gap:12px; margin-top:15px; padding-top:15px; border-top:1px solid #555;">
            <!-- RX AUDIO GAIN (RADIO RX) -->
            <div style="display:flex; flex-direction:column; gap:6px; background:rgba(0,0,0,0.3); padding:10px 12px; border-radius:8px; border:1px solid #4CAF50;">
                <label style="display:flex; justify-content:space-between; align-items:center; font-weight:bold; color:#4CAF50; font-size:13px;">
                    <span>🔊 RX Audio Gain (Radio RX):</span>
                    <span id="rx-gain-val" style="font-family:monospace; font-size:14px; color:#fff; background:#2e7d32; padding:2px 8px; border-radius:4px;">${parseFloat(savedRxGain).toFixed(1)}x</span>
                </label>
                <input type="range" id="rx-gain-slider" min="0" max="10.0" step="0.2" value="${savedRxGain}" oninput="window.updateRxGain()" style="width: 100%; accent-color: #4CAF50; cursor:pointer;">
                <div style="display:flex; justify-content:space-between; font-size:10px; color:#aaa; user-select:none; padding:0 2px;">
                    <span onclick="window.updateRxGain(0)" style="cursor:pointer;" title="0x (Mute)">0x</span>
                    <span onclick="window.updateRxGain(2)" style="cursor:pointer;" title="2.0x">2x</span>
                    <span onclick="window.updateRxGain(4)" style="cursor:pointer;" title="4.0x">4x</span>
                    <span onclick="window.updateRxGain(6)" style="cursor:pointer;" title="6.0x">6x</span>
                    <span onclick="window.updateRxGain(8)" style="cursor:pointer;" title="8.0x">8x</span>
                    <span onclick="window.updateRxGain(10)" style="cursor:pointer;" title="10.0x (Max)">10x (Max)</span>
                </div>
            </div>

            <!-- MICROPHONE SECTION (TX) -->
            <div style="display:flex; flex-direction:column; gap:5px;">
                <label style="font-weight:bold; color:#2196F3; font-size:12px; text-transform:uppercase;">🎙️ Microphone Settings (TX)</label>
                <div style="display:flex; gap:5px;">
                    <select id="mic-select" onchange="window.changeMicrophone()" style="padding:8px; border-radius:5px; color:black; flex-grow:1; max-width: calc(100% - 80px);"></select>
                    <button onclick="window.refreshMics(this)" style="padding:8px; background:#2196F3; color:white; border:none; border-radius:5px; cursor:pointer; width:75px;">Refresh</button>
                </div>
            </div>
            
            <div style="display:flex; flex-direction:column; gap:5px;">
                <label style="display:flex; justify-content: space-between;"><span>Microphone Gain (TX):</span> <span id="mic-gain-val">${parseFloat(savedGain).toFixed(1)}x</span></label>
                <input type="range" id="mic-gain-slider" min="0.1" max="10.0" step="0.1" value="${savedGain}" oninput="window.updateMicEffects()" style="width: 100%;">
            </div>
            
            <label style="display:flex; align-items:center; gap:8px; cursor:pointer; padding: 3px 0;">
                <input type="checkbox" id="mic-comp-enable" onchange="window.updateMicRouting()" style="transform: scale(1.3);" ${savedComp}> Enable Speech Compressor
            </label>
            
            <div style="display:grid; grid-template-columns: 1fr 1fr; gap:10px; margin-top:5px;">
                <div style="display:flex; flex-direction:column; gap:5px;">
                    <label>TX Power</label>
                    <select id="tx-power" onchange="window.syncRadioProfile();" style="padding:8px; border-radius:5px; color:black;"><option value="0">Low</option><option value="1">Low-Mid</option><option value="2">Mid</option><option value="3">Mid-High</option><option value="6" selected>Max</option></select>
                </div>
                <div style="display:flex; flex-direction:column; gap:5px;">
                    <label>Shift (MHz)</label>
                    <div style="display:flex; gap:5px;">
                        <select id="tx-off-dir" onchange="window.syncRadioProfile();" style="padding:8px; border-radius:5px; color:black;"><option value="0">x</option><option value="1">+</option><option value="2">-</option></select>
                        <input type="number" id="tx-off-val" value="0.00" step="0.01" onchange="window.syncRadioProfile();" style="padding:8px; border-radius:5px; color:black; flex-grow:1; width: 100%;">
                    </div>
                </div>
                <div style="display:flex; flex-direction:column; gap:5px;">
                    <label>CTCSS</label>
                    <select id="tx-ctcss" onchange="window.syncRadioProfile();" style="padding:8px; border-radius:5px; color:black;">${ctcssOptions}</select>
                </div>
                <div style="display:flex; flex-direction:column; gap:5px;">
                    <label>DCS</label>
                    <select id="tx-dcs" onchange="window.syncRadioProfile();" style="padding:8px; border-radius:5px; color:black;">${dcsOptions}</select>
                </div>
            </div>
        </div>
    `;
    document.body.appendChild(panel);
}

async function initMicrophone() {
    let btnTxt = document.getElementById('ptt-text');
    let container = document.getElementById('ptt-container');

    if (!window.isSecureContext || !navigator.mediaDevices || !navigator.mediaDevices.getUserMedia) {
        micInitInProgress = false;
        console.error("[Microphone] Insecure Context! navigator.mediaDevices is blocked because the page is loaded over HTTP (" + window.location.origin + "). Web browsers require HTTPS for microphone access.");
        if (btnTxt && container) {
            btnTxt.innerText = "HTTPS required for Mic!";
            container.style.backgroundColor = "#f44336";
        }
        alert("Microphone Error: Browser blocked microphone access because OpenWebRX is loaded over HTTP (" + window.location.origin + ").\n\nWeb browsers strictly disable microphone access on non-HTTPS pages.\n\nTo use microphone in OpenWebRX:\n1. Open chrome://flags/#unsafely-treat-insecure-origin-as-secure in Chrome/Edge, add '" + window.location.origin + "' and click Relaunch.\n2. Or configure OpenWebRX with HTTPS.\n3. Or use the native web interface at https://" + _backendOrigin.host + "/");
        return;
    }

    try {
        let constraints = { audio: { echoCancellation: false, noiseSuppression: false, autoGainControl: false, channelCount: 1 } };
        let ms = document.getElementById('mic-select');
        if (ms && ms.value) constraints.audio.deviceId = { exact: ms.value };
        micStream = await navigator.mediaDevices.getUserMedia(constraints);
        
        if (!audioContext) audioContext = new (window.AudioContext || window.webkitAudioContext)({ sampleRate: 48000 });
        if (audioContext.state === 'suspended') await audioContext.resume();
        
        await loadAudioWorklets();
        initRxPlayer(); 

        if (!opusEncoder) {
            opusEncoder = new AudioEncoder({
                output: (chunk, metadata) => {
                    let opusData = new Uint8Array(chunk.byteLength);
                    chunk.copyTo(opusData);
                    if (isTransmitting && txSocket && txSocket.readyState === WebSocket.OPEN) {
                        txSocket.send(opusData.buffer);
                    }
                },
                error: (e) => console.error("[Opus TX] Encoder error:", e)
            });
            opusEncoder.configure({ codec: 'opus', sampleRate: 48000, numberOfChannels: 1, bitrate: 32000 });
        }

        window.micSourceNode = audioContext.createMediaStreamSource(micStream);
        if (!window.micGainNode) window.micGainNode = audioContext.createGain();
        if (!window.micCompressorNode) {
            window.micCompressorNode = audioContext.createDynamicsCompressor();
            window.micCompressorNode.threshold.value = -40; window.micCompressorNode.knee.value = 30;      
            window.micCompressorNode.ratio.value = 8; window.micCompressorNode.attack.value = 0.005; 
            window.micCompressorNode.release.value = 0.1;  
        }

        window.micAnalyser = audioContext.createAnalyser();
        window.micAnalyser.fftSize = 64; 
        
        if (window.txWorkletNode) { window.txWorkletNode.disconnect(); }
        
        window.txWorkletNode = new AudioWorkletNode(audioContext, 'tx-processor');
        window.txWorkletNode.port.onmessage = (e) => {
            let inputData = e.data;
            if (isTransmitting) {
                let newBuffer = new Float32Array(txPcmBuffer.length + inputData.length);
                newBuffer.set(txPcmBuffer, 0);
                newBuffer.set(inputData, txPcmBuffer.length);
                txPcmBuffer = newBuffer;

                while (txPcmBuffer.length >= 960) {
                    let frameData = txPcmBuffer.slice(0, 960);
                    txPcmBuffer = txPcmBuffer.slice(960);

                    let audioData = new AudioData({
                        format: 'f32-planar', sampleRate: 48000, numberOfFrames: 960,
                        numberOfChannels: 1, timestamp: txTimestamp, data: frameData
                    });
                    txTimestamp += 20000; 
                    opusEncoder.encode(audioData);
                    audioData.close();
                }
            } else {
                txPcmBuffer = new Float32Array(0);
            }
        };
        window.txWorkletNode.connect(audioContext.destination);
        
        window.updateMicRouting();
        window.updateMicEffects();

        micReady = true;
        micInitInProgress = false;
        
        updateConnectionState(isConnected);
        requestAnimationFrame(window.drawSpectrum);
    } catch (err) {
        micInitInProgress = false;
        let btnTxt = document.getElementById('ptt-text');
        let container = document.getElementById('ptt-container');
        if (btnTxt && container) { btnTxt.innerText = "Microphone permission error!"; container.style.backgroundColor = "#f44336"; }
    }
}

function getActiveProfile() {
    let currentFreq = typeof window.currentFreq !== 'undefined' ? window.currentFreq : 145500000;
    let currentMod = typeof window.currentMod !== 'undefined' ? window.currentMod : "FM";
    
    let hash = window.location.hash;
    if (hash) {
        let freqMatch = hash.match(/freq=([0-9]+)/);
        if (freqMatch) currentFreq = parseInt(freqMatch[1]);
        else if (typeof receiver !== 'undefined' && receiver.center_frequency > 0) currentFreq = receiver.center_frequency + receiver.offset_frequency;
        let modMatch = hash.match(/mod=([a-zA-Z0-9]+)/i);
        if (modMatch) currentMod = modMatch[1].toUpperCase();
    } else if (typeof receiver !== 'undefined' && receiver.center_frequency > 0) {
        currentFreq = receiver.center_frequency + receiver.offset_frequency;
        currentMod = receiver.demodulator ? receiver.demodulator.toUpperCase() : "FM";
    }
    
    if (currentFreq === 0) return null;
    return { freq: currentFreq, mod: currentMod };
}

window.updateCompactSMeter = function(dbm, sql) {
    if (isTransmitting) return;

    let sUnit = 0; let over9 = 0;
    if (dbm >= -73) { sUnit = 9; over9 = dbm - (-73); } 
    else if (dbm >= -127) { sUnit = Math.max(0, Math.floor((dbm + 127) / 6)); }

    const cVal = document.getElementById('compact-smeter-val');
    if (cVal) {
        cVal.innerText = (over9 > 0) ? `+${Math.round(over9)}` : `S${sUnit}`;
        if (sql === 1) {
            cVal.style.color = (over9 > 0 || sUnit >= 8) ? '#ff1744' : '#4CAF50';
            cVal.style.textShadow = (over9 > 0 || sUnit >= 8) ? '0 0 6px #ff1744' : '0 0 6px #4CAF50';
        } else {
            cVal.style.color = '#888';
            cVal.style.textShadow = 'none';
        }
    }
    for (let i = 1; i <= 9; i++) {
        const cBar = document.getElementById('compact-sbar-' + i);
        if (cBar) {
            if (i <= sUnit) {
                if (sql === 1) {
                    cBar.style.backgroundColor = (i >= 8) ? '#ff1744' : '#4CAF50';
                    cBar.style.boxShadow = (i >= 8) ? '0 0 4px #ff1744' : '0 0 4px #4CAF50';
                } else {
                    cBar.style.backgroundColor = '#666';
                    cBar.style.boxShadow = 'none';
                }
            } else {
                cBar.style.backgroundColor = '#333';
                cBar.style.boxShadow = 'none';
            }
        }
    }
};

function setTxState(state) {
    if (!isConnected || !txSocket || txSocket.readyState !== WebSocket.OPEN) return;
    if (!micReady) {
        if (state === true && !micInitInProgress) {
            micInitInProgress = true;
            let btnTxt = document.getElementById('ptt-text');
            let container = document.getElementById('ptt-container');
            if(btnTxt && container) { container.style.backgroundColor = "#ff9800"; btnTxt.innerText = "Allow microphone in browser..."; }
            initMicrophone();
        }
        return; 
    }

    if (state === isTransmitting) return;

    isTransmitting = state;

    // Automatically mutes WebRTC audio during transmit!
    if (typeof window.updateAudioMute === 'function') {
        window.updateAudioMute();
    } else if (remoteAudioEl) {
        if (!rxSourceNode) {
            remoteAudioEl.muted = isTransmitting;
        } else {
            remoteAudioEl.muted = true;
        }
    }

    let volSlider = document.getElementById('openwebrx-panel-volume');
    let profile = getActiveProfile();

    if (isTransmitting) {
        updateConnectionState(true);
        if (volSlider) {
            window.preTxVolume = volSlider.value;
            volSlider.value = 0; 
            if (typeof UI !== 'undefined' && UI.setVolume) UI.setVolume(0);
        }

        const cVal = document.getElementById('compact-smeter-val');
        if (cVal) { cVal.innerText = "TX"; cVal.style.color = "#ff1744"; cVal.style.textShadow = "0 0 8px #ff1744"; }
        for (let i = 1; i <= 9; i++) {
            const cBar = document.getElementById('compact-sbar-' + i);
            if (cBar) { cBar.style.backgroundColor = '#ff1744'; cBar.style.boxShadow = '0 0 4px #ff1744'; }
        }
        
        let targetFreq = profile.freq;
        let pwrSelect = document.getElementById('tx-power');
        let ctcssSelect = document.getElementById('tx-ctcss');
        let dcsSelect = document.getElementById('tx-dcs');
        
        let shiftDirSelect = document.getElementById('tx-off-dir');
        let shiftValInput = document.getElementById('tx-off-val');
        let shiftDir = shiftDirSelect ? parseInt(shiftDirSelect.value) : 0;
        let shiftValMHz = shiftValInput ? parseFloat(shiftValInput.value) : 0;
        let shiftHz = Math.round(shiftValMHz * 1000000);
        
        if (shiftDir === 1) targetFreq += shiftHz;
        else if (shiftDir === 2) targetFreq -= shiftHz;

        txSocket.send(JSON.stringify({
            cmd: "start_tx", base_freq: profile.freq, freq: targetFreq, mod: profile.mod,
            pwr: pwrSelect ? parseInt(pwrSelect.value) : 7,
            ctcss: ctcssSelect ? parseFloat(ctcssSelect.value) : 0, dcs: dcsSelect ? parseInt(dcsSelect.value) : 0,
            shift_dir: shiftDir, shift_val: shiftValMHz
        }));
    } else {
        updateConnectionState(true);
        if (volSlider && window.preTxVolume !== null) {
            volSlider.value = window.preTxVolume; 
            if (typeof UI !== 'undefined' && UI.setVolume) UI.setVolume(window.preTxVolume); 
            window.preTxVolume = null;
        }

        window.updateCompactSMeter(-127, 0);

        let ctcssSelect = document.getElementById('tx-ctcss');
        let dcsSelect = document.getElementById('tx-dcs');
        
        txSocket.send(JSON.stringify({ 
            cmd: "stop_tx", freq: profile.freq, mod: profile.mod,
            ctcss: ctcssSelect ? parseFloat(ctcssSelect.value) : 0, dcs: dcsSelect ? parseInt(dcsSelect.value) : 0
        }));
        
        setTimeout(() => { if (typeof Waterfall !== 'undefined' && typeof Waterfall.setAutoRange === 'function') Waterfall.setAutoRange(); }, 800);
    }
}

function createPttButton() {
    if (document.getElementById('ptt-container')) document.getElementById('ptt-container').remove();
    if (document.getElementById('ptt-button')) document.getElementById('ptt-button').remove(); 
    
    let container = document.createElement("div");
    container.id = "ptt-container";
    Object.assign(container.style, {
        position: "fixed", bottom: "20px", left: "50%", transform: "translateX(-50%)",
        display: "flex", width: "90%", maxWidth: "420px", height: "65px", zIndex: "9999",
        boxShadow: "0px 4px 6px rgba(0,0,0,0.3)", borderRadius: "10px", backgroundColor: "#555",
        overflow: "hidden" 
    });

    let btn = document.createElement("button");
    btn.id = "ptt-button"; 
    btn.innerHTML = `<canvas id="ptt-spectrum" style="position:absolute; top:0; left:0; width:100%; height:100%; pointer-events:none;"></canvas><span id="ptt-text" style="position:relative; z-index:1; text-shadow: 1px 1px 3px rgba(0,0,0,0.8);">Loading...</span>`;
    Object.assign(btn.style, { 
        flexGrow: "1", fontSize: "20px", fontWeight: "bold", color: "white", 
        backgroundColor: "transparent", border: "none", 
        cursor: "not-allowed", position: "relative",
        userSelect: "none", touchAction: "none", display: "flex", alignItems: "center", justifyContent: "center"
    });
    
    let stickyBtn = document.createElement("button");
    stickyBtn.id = "ptt-sticky";
    stickyBtn.innerHTML = "🔓"; 
    Object.assign(stickyBtn.style, {
        width: "65px", backgroundColor: "rgba(0,0,0,0.2)", border: "none", borderLeft: "1px solid rgba(0,0,0,0.4)",
        color: "white", cursor: "pointer", fontSize: "26px",
        display: "flex", alignItems: "center", justifyContent: "center", transition: "background 0.3s"
    });

    const handleDown = (e) => { e.preventDefault(); if(!window.isStickyPtt) setTxState(true); };
    const handleUp = (e) => { e.preventDefault(); if(!window.isStickyPtt) setTxState(false); };
    const handleLeave = (e) => { e.preventDefault(); if(!window.isStickyPtt) setTxState(false); };

    btn.addEventListener("mousedown", handleDown);
    btn.addEventListener("mouseup", handleUp);
    btn.addEventListener("mouseleave", handleLeave);
    btn.addEventListener("touchstart", handleDown, {passive: false});
    btn.addEventListener("touchend", handleUp, {passive: false});

    stickyBtn.addEventListener("click", (e) => {
        e.preventDefault();
        if (!isConnected) return;
        
        if (!micReady) {
            setTxState(true); 
            return;
        }

        window.isStickyPtt = !window.isStickyPtt;
        if (window.isStickyPtt) {
            stickyBtn.innerHTML = "🔒";
            stickyBtn.style.backgroundColor = "rgba(0,0,0,0.5)";
            setTxState(true);
        } else {
            stickyBtn.innerHTML = "🔓";
            stickyBtn.style.backgroundColor = "rgba(0,0,0,0.2)";
            setTxState(false);
        }
    });

    container.appendChild(btn);
    container.appendChild(stickyBtn);
    document.body.appendChild(container);
}

document.addEventListener("keydown", (e) => { 
    if (e.keyCode === 32 && e.target.tagName !== "INPUT" && e.target.tagName !== "SELECT") { 
        e.preventDefault(); 
        if(!window.isStickyPtt) setTxState(true); 
    } 
});
document.addEventListener("keyup", (e) => { 
    if (e.keyCode === 32 && e.target.tagName !== "INPUT" && e.target.tagName !== "SELECT") { 
        e.preventDefault(); 
        if(!window.isStickyPtt) setTxState(false); 
    } 
});

window.applyProfileSettings = function() {
    try {
        let bookmarkName = ""; let nameMatch = window.location.hash.match(/name=([^&]+)/);
        if (nameMatch) bookmarkName = decodeURIComponent(nameMatch[1]);
        let sdrProfileText = ""; let profileSelect = document.getElementById("openwebrx-sdr-profiles-listbox");
        if (profileSelect && profileSelect.selectedIndex >= 0) sdrProfileText = profileSelect.options[profileSelect.selectedIndex].text;
        let targetConfig = bookmarkName.includes(";") ? bookmarkName : (sdrProfileText.includes(";") ? sdrProfileText : (bookmarkName || sdrProfileText));

        if (targetConfig !== lastProfileName) {
            lastProfileName = targetConfig; 
            let parts = targetConfig.split(';');
            if (parts.length >= 3) {
                let ctcssVal = parseFloat(parts[1].replace(',', '.').trim());
                let shiftRaw = parseFloat(parts[2].replace(',', '.').trim()); 
                
                let ctcssSelect = document.getElementById('tx-ctcss');
                if (ctcssSelect && !isNaN(ctcssVal)) {
                    let found = false;
                    for (let i = 0; i < ctcssSelect.options.length; i++) {
                        if (parseFloat(ctcssSelect.options[i].value) === ctcssVal) { ctcssSelect.selectedIndex = i; found = true; break; }
                    }
                    if (!found) {
                        let opt = document.createElement('option'); opt.value = ctcssVal; opt.innerHTML = ctcssVal + " Hz";
                        ctcssSelect.appendChild(opt); ctcssSelect.value = ctcssVal;
                    }
                }

                let shiftDirSelect = document.getElementById('tx-off-dir');
                let shiftValInput = document.getElementById('tx-off-val');
                if (shiftDirSelect && shiftValInput && !isNaN(shiftRaw)) {
                    if (shiftRaw < 0) { shiftDirSelect.value = "2"; shiftValInput.value = Math.abs(shiftRaw).toFixed(2); } 
                    else if (shiftRaw > 0) { shiftDirSelect.value = "1"; shiftValInput.value = shiftRaw.toFixed(2); } 
                    else { shiftDirSelect.value = "0"; shiftValInput.value = "0.00"; }
                }
            } else {
                let shiftDirSelect = document.getElementById('tx-off-dir');
                let shiftValInput = document.getElementById('tx-off-val');
                let ctcssSelect = document.getElementById('tx-ctcss');
                let dcsSelect = document.getElementById('tx-dcs');

                if (shiftDirSelect) shiftDirSelect.value = "0";
                if (shiftValInput) shiftValInput.value = "0.00";
                if (ctcssSelect) ctcssSelect.value = "0";
                if (dcsSelect) dcsSelect.value = "0";
            }
        }
        window.syncRadioProfile();
    } catch(e) { }
};

window.addEventListener("hashchange", () => { setTimeout(window.applyProfileSettings, 300); });

document.addEventListener('click', (e) => { 
    if (!wakeLock) requestWakeLock(); 
    if (audioContext && audioContext.state === 'suspended') audioContext.resume(); 
    if (e.target && (e.target.className.includes('openwebrx-bookmark') || e.target.tagName === 'OPTION')) {
        setTimeout(window.applyProfileSettings, 300);
    }
});

setInterval(() => { let profile = getActiveProfile(); if (profile && profile.freq !== lastPolledFreq) { lastPolledFreq = profile.freq; if(isConnected) window.syncRadioProfile(); } }, 1000);

let heartbeatInterval; 

function initTxPlugin() {
    const wsUrl = _backendOrigin.wsProto + _backendOrigin.host + "/tx-ws/";
    console.log("[owrx.js] Connecting CAT WebSocket to:", wsUrl);
    try {
        txSocket = new WebSocket(wsUrl);
        txSocket.binaryType = "arraybuffer";
    } catch(e) {
        console.error("[owrx.js] Failed to create WebSocket to " + wsUrl, e);
        updateConnectionState(false);
        setTimeout(initTxPlugin, 5000);
        return;
    }

    txSocket.onopen = () => {
        console.log("[owrx.js] CAT WebSocket connected successfully to:", wsUrl);
        updateConnectionState(true);
        
        // Fast frequency sync after connection so backend knows the band immediately
        setTimeout(window.applyProfileSettings, 200); 
        
        let mainSelect = document.getElementById('openwebrx-sdr-profiles-listbox');
        if(mainSelect) mainSelect.addEventListener('change', () => { setTimeout(window.applyProfileSettings, 100); });
        heartbeatInterval = setInterval(() => { if (txSocket && txSocket.readyState === WebSocket.OPEN) txSocket.send(JSON.stringify({ cmd: "heartbeat" })); }, 30000);
        
        // Protected autostart RX - Give backend 1 second in case it needs to stop Direwolf
        if (localStorage.getItem('tx_rx_audio') === "1") {
            setTimeout(() => {
                let rxToggle = document.getElementById('rx-audio-toggle');
                if (rxToggle) rxToggle.checked = true;
                window.toggleRxAudio(true);
            }, 1000);
        }
    };

    txSocket.onmessage = (event) => {
        if (typeof event.data === "string") {
            try {
                let msg = JSON.parse(event.data);
                if (msg.cmd === "sql_state") {
                    window.sqlOpen = msg.open;
                    let rxLabel = document.getElementById('rx-audio-label');
                    if (rxLabel) {
                        if (rxAudioEnabled && webrtcPC) rxLabel.style.backgroundColor = "#4CAF50";
                        rxLabel.style.color = msg.open ? "#ff1744" : "#fff";
                        rxLabel.style.textShadow = msg.open ? "0 0 10px #ff0000, 0 0 16px #ff1744" : "none";
                        rxLabel.style.boxShadow = msg.open ? "0 0 12px rgba(255, 23, 68, 0.85)" : "none";
                    }
                } else if (msg.cmd === "scan_result" || msg.cmd === "s_meter") {
                    let isOpen = (msg.sql === 1);
                    window.sqlOpen = isOpen;
                    let rxLabel = document.getElementById('rx-audio-label');
                    if (rxLabel) {
                        if (rxAudioEnabled && webrtcPC) rxLabel.style.backgroundColor = "#4CAF50";
                        rxLabel.style.color = isOpen ? "#ff1744" : "#fff";
                        rxLabel.style.textShadow = isOpen ? "0 0 10px #ff0000, 0 0 16px #ff1744" : "none";
                        rxLabel.style.boxShadow = isOpen ? "0 0 12px rgba(255, 23, 68, 0.85)" : "none";
                    }
                    if (typeof window.updateCompactSMeter === 'function') {
                        window.updateCompactSMeter(msg.dbm, msg.sql);
                    }
                } else if (msg.cmd === "scan_status") {
                    let scanToggle = document.getElementById('scan-toggle');
                    let scanLabel = document.getElementById('scan-label');
                    if (scanToggle) scanToggle.checked = msg.active;
                    if (scanLabel) scanLabel.style.backgroundColor = msg.active ? "#2196F3" : "#444";
                } else if (msg.cmd === "audio_system_status") {
                    // Response from backend to check_audio_system
                    if (msg.ready && rxAudioEnabled) {
                        startWebRTCConnection(); // Audio system is ready, connect!
                    } else if (!msg.ready && rxAudioEnabled) {
                        console.log("[WebRTC] MediaMTX is busy with Direwolf or resetting. Waiting...");
                        // Recheck in 1.5 seconds
                        setTimeout(() => {
                            if (rxAudioEnabled && txSocket && txSocket.readyState === WebSocket.OPEN) {
                                txSocket.send(JSON.stringify({ cmd: "check_audio_system" }));
                            }
                        }, 1500);
                    }
                }
            } catch(e) {}
        }
    };

    txSocket.onclose = (ev) => { 
        console.warn("[owrx.js] WebSocket disconnected from " + wsUrl, ev);
        updateConnectionState(false); 
        clearInterval(heartbeatInterval); 
        setTimeout(initTxPlugin, 5000); 
    };
    txSocket.onerror = (err) => { 
        console.error("[owrx.js] WebSocket error connecting to " + wsUrl + ". Note: If using self-signed HTTPS certificate on port 8443, open https://" + _backendOrigin.host + " in a browser tab first and accept the certificate exception.", err);
        updateConnectionState(false); 
    };
}

if (navigator.mediaDevices && typeof navigator.mediaDevices.ondevicechange !== 'undefined') navigator.mediaDevices.ondevicechange = async () => { await populateMics(); };

function startPlugin() {
    if (window.__owrx_cat_injected) return;
    window.__owrx_cat_injected = true;

    createPttButton();
    createTxPanel();
    updateConnectionState(false); 
    initTxPlugin();

    if (localStorage.getItem('tx_rx_audio') === "1") {
        window.toggleRxAudio(true);
    }
}

if (document.readyState === "complete" || document.readyState === "interactive") {
    startPlugin();
} else {
    window.addEventListener("load", startPlugin);
}

// Safeguard audio resumption for native audio tag (WebRTC)
document.body.addEventListener('click', function() {
    if (remoteAudioEl && remoteAudioEl.paused && rxAudioEnabled && !isTransmitting) {
        remoteAudioEl.play().catch(()=>{});
    }
}, { once: false });