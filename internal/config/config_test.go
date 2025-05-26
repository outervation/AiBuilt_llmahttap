package config

import (
	"io/ioutil"
	"os"
	"strings"
	"testing"
)

// writeTempFile creates a temporary file with the given content and extension.
// It returns the path to the file and a cleanup function to remove the file.
func writeTempFile(t *testing.T, content string, ext string) (path string, cleanup func()) {
	t.Helper()
	tmpFile, err := ioutil.TempFile("", "test-config-*"+ext)
	if err != nil {
		t.Fatalf("Failed to create temp file: %v", err)
	}

	if _, err := tmpFile.WriteString(content); err != nil {
		tmpFile.Close()
		os.Remove(tmpFile.Name())
		t.Fatalf("Failed to write to temp file: %v", err)
	}

	if err := tmpFile.Close(); err != nil {
		os.Remove(tmpFile.Name())
		t.Fatalf("Failed to close temp file: %v", err)
	}

	return tmpFile.Name(), func() {
		os.Remove(tmpFile.Name())
	}
}

// checkErrorContains checks if the error is not nil and its message contains the expected substring.
func checkErrorContains(t *testing.T, err error, expectedSubstring string) {
	t.Helper()
	if err == nil {
		t.Fatalf("Expected an error containing %q, but got nil", expectedSubstring)
	}
	if !strings.Contains(err.Error(), expectedSubstring) {
		t.Fatalf("Expected error message to contain %q, but got: %v", expectedSubstring, err)
	}
}

func TestLoadConfig_EmptyPath(t *testing.T) {
	_, err := LoadConfig("")
	checkErrorContains(t, err, "configuration file path cannot be empty")
}

func TestLoadConfig_NonExistentFile(t *testing.T) {
	_, err := LoadConfig("non_existent_file.json")
	checkErrorContains(t, err, "failed to read configuration file")
}

func TestLoadConfig_ValidJSON(t *testing.T) {
	content := `{"server": {"address": ":8080"}}`
	path, cleanup := writeTempFile(t, content, ".json")
	defer cleanup()

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig failed for valid JSON: %v", err)
	}
	if cfg == nil {
		t.Fatal("Expected config to be non-nil for valid JSON")
	}
	if cfg.Server == nil || cfg.Server.Address == nil || *cfg.Server.Address != ":8080" {
		t.Errorf("Expected server address to be :8080, got %v", cfg.Server)
	}
}

func TestLoadConfig_ValidTOML(t *testing.T) {
	content := `
[server]
address = ":8081"
`
	path, cleanup := writeTempFile(t, content, ".toml")
	defer cleanup()

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig failed for valid TOML: %v", err)
	}
	if cfg == nil {
		t.Fatal("Expected config to be non-nil for valid TOML")
	}
	if cfg.Server == nil || cfg.Server.Address == nil || *cfg.Server.Address != ":8081" {
		t.Errorf("Expected server address to be :8081, got %v", cfg.Server)
	}
}

func TestLoadConfig_AutoDetectJSON(t *testing.T) {
	content := `{"logging": {"log_level": "DEBUG"}}`
	path, cleanup := writeTempFile(t, content, ".conf") // Unknown extension
	defer cleanup()

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig failed for auto-detect JSON: %v", err)
	}
	if cfg == nil {
		t.Fatal("Expected config to be non-nil for auto-detect JSON")
	}
	if cfg.Logging == nil || cfg.Logging.LogLevel != LogLevelDebug {
		t.Errorf("Expected log level to be DEBUG, got %v", cfg.Logging)
	}
}

func TestLoadConfig_AutoDetectTOML(t *testing.T) {
	content := `
[logging]
log_level = "WARNING"
`
	path, cleanup := writeTempFile(t, content, ".cfg") // Unknown extension
	defer cleanup()

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig failed for auto-detect TOML: %v", err)
	}
	if cfg == nil {
		t.Fatal("Expected config to be non-nil for auto-detect TOML")
	}
	if cfg.Logging == nil || cfg.Logging.LogLevel != LogLevelWarning {
		t.Errorf("Expected log level to be WARNING, got %v", cfg.Logging)
	}
}

func TestLoadConfig_AutoDetectFailure(t *testing.T) {
	content := `not json or toml`
	path, cleanup := writeTempFile(t, content, ".data") // Unknown extension
	defer cleanup()

	_, err := LoadConfig(path)
	checkErrorContains(t, err, "failed to auto-detect and parse config")
	checkErrorContains(t, err, "JSON error")
	checkErrorContains(t, err, "TOML error")
}

func TestLoadConfig_InvalidJSONSyntax(t *testing.T) {
	content := `{"server": {"address": ":8080",}}` // Trailing comma
	path, cleanup := writeTempFile(t, content, ".json")
	defer cleanup()

	_, err := LoadConfig(path)
	checkErrorContains(t, err, "failed to parse JSON config")
}

func TestLoadConfig_InvalidTOMLSyntax(t *testing.T) {
	content := `
[server
address = ":8080"
` // Missing closing bracket
	path, cleanup := writeTempFile(t, content, ".toml")
	defer cleanup()

	_, err := LoadConfig(path)
	checkErrorContains(t, err, "failed to parse TOML config")
}

func TestLoadConfig_DefaultsApplied(t *testing.T) {
	content := `{}` // Empty JSON
	path, cleanup := writeTempFile(t, content, ".json")
	defer cleanup()

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig failed for empty JSON: %v", err)
	}
	if cfg == nil {
		t.Fatal("Expected config to be non-nil")
	}

	// Check server defaults
	if cfg.Server == nil {
		t.Fatal("cfg.Server is nil after defaults")
	}
	if cfg.Server.Address == nil || *cfg.Server.Address != defaultServerAddress {
		t.Errorf("Expected default server address %s, got %v", defaultServerAddress, cfg.Server.Address)
	}
	if cfg.Server.ChildReadinessTimeout == nil || *cfg.Server.ChildReadinessTimeout != defaultChildReadinessTimeout {
		t.Errorf("Expected default child readiness timeout %s, got %v", defaultChildReadinessTimeout, cfg.Server.ChildReadinessTimeout)
	}
	if cfg.Server.GracefulShutdownTimeout == nil || *cfg.Server.GracefulShutdownTimeout != defaultGracefulShutdownTimeout {
		t.Errorf("Expected default graceful shutdown timeout %s, got %v", defaultGracefulShutdownTimeout, cfg.Server.GracefulShutdownTimeout)
	}

	// Check logging defaults
	if cfg.Logging == nil {
		t.Fatal("cfg.Logging is nil after defaults")
	}
	if cfg.Logging.LogLevel != defaultLogLevel {
		t.Errorf("Expected default log level %s, got %s", defaultLogLevel, cfg.Logging.LogLevel)
	}
	if cfg.Logging.AccessLog == nil {
		t.Fatal("cfg.Logging.AccessLog is nil after defaults")
	}
	if cfg.Logging.AccessLog.Enabled == nil || *cfg.Logging.AccessLog.Enabled != defaultAccessLogEnabled {
		t.Errorf("Expected default access log enabled %t, got %v", defaultAccessLogEnabled, cfg.Logging.AccessLog.Enabled)
	}
	if cfg.Logging.AccessLog.Target != defaultAccessLogTarget {
		t.Errorf("Expected default access log target %s, got %s", defaultAccessLogTarget, cfg.Logging.AccessLog.Target)
	}
	if cfg.Logging.AccessLog.Format != defaultAccessLogFormat {
		t.Errorf("Expected default access log format %s, got %s", defaultAccessLogFormat, cfg.Logging.AccessLog.Format)
	}
	if cfg.Logging.AccessLog.RealIPHeader == nil || *cfg.Logging.AccessLog.RealIPHeader != defaultAccessLogRealIPHeader {
		t.Errorf("Expected default access log real_ip_header %s, got %v", defaultAccessLogRealIPHeader, cfg.Logging.AccessLog.RealIPHeader)
	}
	if cfg.Logging.ErrorLog == nil {
		t.Fatal("cfg.Logging.ErrorLog is nil after defaults")
	}
	if cfg.Logging.ErrorLog.Target != defaultErrorLogTarget {
		t.Errorf("Expected default error log target %s, got %s", defaultErrorLogTarget, cfg.Logging.ErrorLog.Target)
	}

	// Check routing defaults
	if cfg.Routing == nil {
		t.Fatal("cfg.Routing is nil after defaults")
	}
	if cfg.Routing.Routes == nil { // Should be initialized to empty slice
		t.Fatal("cfg.Routing.Routes is nil after defaults")
	}
	if len(cfg.Routing.Routes) != 0 {
		t.Errorf("Expected cfg.Routing.Routes to be empty, got %d routes", len(cfg.Routing.Routes))
	}
}

func TestLoadConfig_SpecificValuesOverrideDefaults(t *testing.T) {
	content := `
{
    "server": {
        "address": "127.0.0.1:9090",
        "child_readiness_timeout": "5s",
        "graceful_shutdown_timeout": "15s"
    },
    "logging": {
        "log_level": "ERROR",
        "access_log": {
            "enabled": false,
            "target": "/var/log/access.log",
            "format": "json",
            "real_ip_header": "CF-Connecting-IP"
        },
        "error_log": {
            "target": "/var/log/error.log"
        }
    }
}
`
	path, cleanup := writeTempFile(t, content, ".json")
	defer cleanup()

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}

	if *cfg.Server.Address != "127.0.0.1:9090" {
		t.Errorf("Server address mismatch: expected 127.0.0.1:9090, got %s", *cfg.Server.Address)
	}
	if *cfg.Server.ChildReadinessTimeout != "5s" {
		t.Errorf("ChildReadinessTimeout mismatch: expected 5s, got %s", *cfg.Server.ChildReadinessTimeout)
	}
	if *cfg.Server.GracefulShutdownTimeout != "15s" {
		t.Errorf("GracefulShutdownTimeout mismatch: expected 15s, got %s", *cfg.Server.GracefulShutdownTimeout)
	}
	if cfg.Logging.LogLevel != LogLevelError {
		t.Errorf("LogLevel mismatch: expected ERROR, got %s", cfg.Logging.LogLevel)
	}
	if *cfg.Logging.AccessLog.Enabled != false {
		t.Errorf("AccessLog.Enabled mismatch: expected false, got %t", *cfg.Logging.AccessLog.Enabled)
	}
	if cfg.Logging.AccessLog.Target != "/var/log/access.log" {
		t.Errorf("AccessLog.Target mismatch: expected /var/log/access.log, got %s", cfg.Logging.AccessLog.Target)
	}
	if *cfg.Logging.AccessLog.RealIPHeader != "CF-Connecting-IP" {
		t.Errorf("AccessLog.RealIPHeader mismatch: expected CF-Connecting-IP, got %s", *cfg.Logging.AccessLog.RealIPHeader)
	}
	if cfg.Logging.ErrorLog.Target != "/var/log/error.log" {
		t.Errorf("ErrorLog.Target mismatch: expected /var/log/error.log, got %s", cfg.Logging.ErrorLog.Target)
	}
}
