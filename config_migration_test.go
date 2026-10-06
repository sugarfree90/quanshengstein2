package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfigMigration(t *testing.T) {
	tmpDir := t.TempDir()
	origCfgFile := cfgFile
	origScriptDir := scriptDir
	defer func() {
		cfgFile = origCfgFile
		scriptDir = origScriptDir
	}()

	cfgFile = filepath.Join(tmpDir, "config.json")
	scriptDir = tmpDir

	// 1. Test missing file creation from embedded/example template
	loaded := loadAppConfig()
	if loaded.Callsign == "" {
		t.Fatalf("expected non-empty callsign")
	}

	data, err := os.ReadFile(cfgFile)
	if err != nil {
		t.Fatalf("failed to read created config.json: %v", err)
	}
	content := string(data)
	if !strings.Contains(content, "// STATION IDENTITY") {
		t.Fatalf("expected comments to be preserved in newly created config.json")
	}

	// 2. Test auto-migration when a user is missing a new setting
	userCustom := `{
    // My custom callsign
    "callsign": "SP3XYZ",
    "serial_port": "/dev/ttyUSB0"
}
`
	if err := os.WriteFile(cfgFile, []byte(userCustom), 0644); err != nil {
		t.Fatalf("failed to write custom config: %v", err)
	}

	// Run migration
	migrateConfigPreservingComments()

	migratedData, err := os.ReadFile(cfgFile)
	if err != nil {
		t.Fatalf("failed to read migrated config: %v", err)
	}
	migratedContent := string(migratedData)

	// Check that custom user values and comments are preserved
	if !strings.Contains(migratedContent, `"callsign": "SP3XYZ"`) {
		t.Errorf("user custom callsign was lost!")
	}
	if !strings.Contains(migratedContent, `"serial_port": "/dev/ttyUSB0"`) {
		t.Errorf("user custom serial port was lost!")
	}
	if !strings.Contains(migratedContent, "// My custom callsign") {
		t.Errorf("user custom comment was lost!")
	}

	// Check that missing keys were added with their template comments
	if !strings.Contains(migratedContent, `"mqtt_broker"`) {
		t.Errorf("missing key mqtt_broker was not added!")
	}
	if !strings.Contains(migratedContent, "// MQTT broker connection URL") {
		t.Errorf("missing key comments were not added!")
	}

	// Check that the migrated content is valid JSON
	clean := stripJSONComments(migratedData)
	var parsed map[string]interface{}
	if err := json.Unmarshal(clean, &parsed); err != nil {
		t.Fatalf("migrated JSON is invalid syntax: %v\nContent:\n%s", err, migratedContent)
	}
	if parsed["callsign"] != "SP3XYZ" {
		t.Errorf("parsed callsign mismatch: got %v", parsed["callsign"])
	}
}
