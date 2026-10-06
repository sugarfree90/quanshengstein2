package main

import (
	"bytes"
	"sync"
	"testing"
	"time"

	"go.bug.st/serial"
)

type mockSerialPort struct {
	mu           sync.Mutex
	written      bytes.Buffer
	dtr          bool
	rts          bool
	closed       bool
	writeWaiters []chan struct{}
}

func (m *mockSerialPort) Read(p []byte) (n int, err error) {
	return 0, nil
}

func (m *mockSerialPort) Write(p []byte) (n int, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n, err = m.written.Write(p)
	for _, ch := range m.writeWaiters {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
	return n, err
}

func (m *mockSerialPort) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	return nil
}

func (m *mockSerialPort) SetMode(mode *serial.Mode) error {
	return nil
}

func (m *mockSerialPort) SetDTR(dtr bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.dtr = dtr
	return nil
}

func (m *mockSerialPort) SetRTS(rts bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rts = rts
	return nil
}

func (m *mockSerialPort) SetReadTimeout(t time.Duration) error {
	return nil
}

func (m *mockSerialPort) ResetInputBuffer() error {
	return nil
}

func (m *mockSerialPort) ResetOutputBuffer() error {
	return nil
}

func (m *mockSerialPort) Drain() error {
	return nil
}

func (m *mockSerialPort) Break(d time.Duration) error {
	return nil
}

func (m *mockSerialPort) GetModemStatusBits() (*serial.ModemStatusBits, error) {
	return &serial.ModemStatusBits{}, nil
}

func (m *mockSerialPort) getWritten() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.written.String()
}

func (m *mockSerialPort) getDTR() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.dtr
}

func (m *mockSerialPort) getRTS() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.rts
}

func TestPTTModeSafe(t *testing.T) {
	mock := &mockSerialPort{}
	cat := &QuanshengCAT{
		portName: "test_port",
		baudRate: 38400,
		port:     mock,
	}

	configLock.Lock()
	appCfg.PTTMode = "safe"
	configLock.Unlock()

	// 1. Engage PTT
	cat.TxOn()

	time.Sleep(50 * time.Millisecond)
	firstCheck := mock.getWritten()
	if firstCheck != "TXS;" {
		t.Fatalf("expected immediate 'TXS;' on TxOn, got '%s'", firstCheck)
	}

	// 2. Wait for keepalive renewal (ticker sends TXS; every 450ms)
	time.Sleep(550 * time.Millisecond)
	secondCheck := mock.getWritten()
	if secondCheck != "TXS;TXS;" {
		t.Fatalf("expected 'TXS;TXS;' after 600ms keepalive, got '%s'", secondCheck)
	}

	// 3. Disengage PTT
	cat.RxOn()

	time.Sleep(50 * time.Millisecond)
	finalCheck := mock.getWritten()
	// Should contain the initial TXS; renewals plus double RX;
	if finalCheck != "TXS;TXS;RX;RX;" {
		t.Fatalf("expected 'TXS;TXS;RX;RX;', got '%s'", finalCheck)
	}

	// 4. Verify ticker stopped (no more TXS; after RxOn)
	time.Sleep(550 * time.Millisecond)
	afterStopCheck := mock.getWritten()
	if afterStopCheck != "TXS;TXS;RX;RX;" {
		t.Fatalf("keepalive ticker did not stop after RxOn! Got '%s'", afterStopCheck)
	}
}

func TestPTTModeLegacy(t *testing.T) {
	mock := &mockSerialPort{}
	cat := &QuanshengCAT{
		portName: "test_port",
		baudRate: 38400,
		port:     mock,
	}

	configLock.Lock()
	appCfg.PTTMode = "legacy"
	configLock.Unlock()

	// 1. Engage PTT
	cat.TxOn()
	time.Sleep(50 * time.Millisecond)
	if got := mock.getWritten(); got != "TX;" {
		t.Fatalf("expected 'TX;', got '%s'", got)
	}

	// 2. Verify no keepalive ticker in legacy mode
	time.Sleep(550 * time.Millisecond)
	if got := mock.getWritten(); got != "TX;" {
		t.Fatalf("legacy mode should not send keepalive! Got '%s'", got)
	}

	// 3. Disengage PTT
	cat.RxOn()
	time.Sleep(50 * time.Millisecond)
	if got := mock.getWritten(); got != "TX;RX;RX;" {
		t.Fatalf("expected 'TX;RX;RX;', got '%s'", got)
	}
}

func TestPTTModeHardware(t *testing.T) {
	mock := &mockSerialPort{}
	cat := &QuanshengCAT{
		portName: "test_port",
		baudRate: 38400,
		port:     mock,
	}

	configLock.Lock()
	appCfg.PTTMode = "hardware"
	configLock.Unlock()

	// 1. Engage PTT
	cat.TxOn()
	time.Sleep(50 * time.Millisecond)
	if !mock.getDTR() {
		t.Fatalf("expected DTR=true in hardware mode")
	}
	if mock.getRTS() {
		t.Fatalf("expected RTS=false in hardware mode")
	}
	if got := mock.getWritten(); got != "" {
		t.Fatalf("hardware mode should not write CAT commands, wrote '%s'", got)
	}

	// 2. Disengage PTT
	cat.RxOn()
	time.Sleep(50 * time.Millisecond)
	if mock.getDTR() {
		t.Fatalf("expected DTR=false after RxOn in hardware mode")
	}
	if mock.getRTS() {
		t.Fatalf("expected RTS=false after RxOn in hardware mode")
	}
}

func TestCATCloseAndReconnectPrevention(t *testing.T) {
	mock := &mockSerialPort{}
	cat := &QuanshengCAT{
		portName: "test_port",
		baudRate: 38400,
		port:     mock,
	}

	cat.Close()

	if !cat.closed.Load() {
		t.Fatalf("expected cat.closed to be true after Close()")
	}

	if cat.port != nil {
		t.Fatalf("expected cat.port to be nil after Close()")
	}

	// Any reconnect attempts should fail immediately without touching hardware
	if cat.Connect() {
		t.Fatalf("Connect() should return false when CAT is closed")
	}

	if _, err := cat.GetSMeter(); err == nil {
		t.Fatalf("GetSMeter() should return error when CAT is closed")
	}

	if _, err := cat.Send("FA00145500000", false); err == nil {
		t.Fatalf("Send() should return error when CAT is closed")
	}

	if _, _, _, err := cat.ScanFast(145500000, 1); err == nil {
		t.Fatalf("ScanFast() should return error when CAT is closed")
	}

	// TxOn and RxOn after Close should be safe no-ops
	cat.TxOn()
	cat.RxOn()
}

func TestSendRawDoesNotDeadlock(t *testing.T) {
	mock := &mockSerialPort{}
	cat := &QuanshengCAT{
		portName: "test_port",
		baudRate: 38400,
		port:     mock,
	}

	done := make(chan struct{})
	go func() {
		// Callers hold cat.lock before calling sendRaw (e.g. tuneToPresetIfExists)
		cat.lock.Lock()
		defer cat.lock.Unlock()

		err := cat.sendRaw("FA00145500000")
		if err != nil {
			t.Errorf("sendRaw error: %v", err)
		}
		close(done)
	}()

	select {
	case <-done:
		// Success, no deadlock!
	case <-time.After(500 * time.Millisecond):
		t.Fatalf("sendRaw deadlocked on cat.lock!")
	}

	if got := mock.getWritten(); got != "FA00145500000;" {
		t.Fatalf("expected 'FA00145500000;', got '%s'", got)
	}
}
