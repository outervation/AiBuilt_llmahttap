package config

import (
	"encoding/json"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BurntSushi/toml"
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

// Helper function to get a pointer to a string.
func strPtr(s string) *string {
	return &s
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

func TestLoadConfig_EmptyFile(t *testing.T) {
	tests := []struct {
		name        string
		ext         string
		expectError string
		// expectJsonError and expectTomlError are for auto-detect cases
		expectJsonError string
		expectTomlError string
	}{
		{
			name:        "empty .json file",
			ext:         ".json",
			expectError: "failed to parse JSON config",
			// More specific check for the actual JSON error
			// json.Unmarshal of empty byte slice gives "unexpected end of JSON input"
			expectJsonError: "unexpected end of JSON input",
		},
		{
			name:        "empty .toml file",
			ext:         ".toml",
			expectError: "failed to parse TOML config",
			// toml.Unmarshal of empty byte slice gives "toml: unmarshal: empty input" or similar
			expectTomlError: "empty input", // toml library specific error message part
		},
		{
			name:            "empty file auto-detect",
			ext:             ".empty", // unknown extension for auto-detect
			expectError:     "failed to auto-detect and parse config",
			expectJsonError: "unexpected end of JSON input",
			expectTomlError: "empty input",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path, cleanup := writeTempFile(t, "", tc.ext) // Empty content
			defer cleanup()

			_, err := LoadConfig(path)
			checkErrorContains(t, err, tc.expectError)

			if tc.expectJsonError != "" {
				checkErrorContains(t, err, tc.expectJsonError)
			}
			if tc.expectTomlError != "" {
				checkErrorContains(t, err, tc.expectTomlError)
			}
		})
	}
}

func TestLoadConfig_InvalidJSONSyntax(t *testing.T) {
	content := `{"server": {"address": ":8080",}}` // Trailing comma
	path, cleanup := writeTempFile(t, content, ".json")
	defer cleanup()

	_, err := LoadConfig(path)
	checkErrorContains(t, err, "failed to parse JSON config")
	// More specific check for the actual JSON error
	checkErrorContains(t, err, "invalid character '}' looking for beginning of object key string") // This is a common error for trailing commas
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
	// More specific check for TOML error
	checkErrorContains(t, err, "expected '.' or ']' to end table name") // Adjusted based on actual error
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

	if cfg.Logging.AccessLog.Target == nil || *cfg.Logging.AccessLog.Target != defaultAccessLogTarget {
		t.Errorf("Expected default access log target %s, got %v", defaultAccessLogTarget, cfg.Logging.AccessLog.Target)
	}
	if cfg.Logging.AccessLog.Format != defaultAccessLogFormat {
		t.Errorf("Expected default access log format %s, got %s", defaultAccessLogFormat, cfg.Logging.AccessLog.Format)
	}
	if cfg.Logging.AccessLog.RealIPHeader == nil || *cfg.Logging.AccessLog.RealIPHeader != defaultAccessLogRealIPHeader {
		t.Errorf("Expected default access log real_ip_header %s, got %v", defaultAccessLogRealIPHeader, cfg.Logging.AccessLog.RealIPHeader)
	}
	if cfg.Logging.AccessLog.TrustedProxies == nil {
		t.Errorf("Expected default access log TrustedProxies to be an empty slice, got nil")
	} else if len(cfg.Logging.AccessLog.TrustedProxies) != 0 {
		t.Errorf("Expected default access log TrustedProxies to be an empty slice, got %v", cfg.Logging.AccessLog.TrustedProxies)
	}
	if cfg.Logging.ErrorLog == nil {
		t.Fatal("cfg.Logging.ErrorLog is nil after defaults")
	}

	if cfg.Logging.ErrorLog.Target == nil || *cfg.Logging.ErrorLog.Target != defaultErrorLogTarget {
		t.Errorf("Expected default error log target %s, got %v", defaultErrorLogTarget, cfg.Logging.ErrorLog.Target)
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

	if cfg.Logging.AccessLog.Target == nil || *cfg.Logging.AccessLog.Target != "/var/log/access.log" {
		t.Errorf("AccessLog.Target mismatch: expected /var/log/access.log, got %v", cfg.Logging.AccessLog.Target)
	}
	if *cfg.Logging.AccessLog.RealIPHeader != "CF-Connecting-IP" {
		t.Errorf("AccessLog.RealIPHeader mismatch: expected CF-Connecting-IP, got %s", *cfg.Logging.AccessLog.RealIPHeader)
	}

	if cfg.Logging.ErrorLog.Target == nil || *cfg.Logging.ErrorLog.Target != "/var/log/error.log" {
		t.Errorf("ErrorLog.Target mismatch: expected /var/log/error.log, got %v", cfg.Logging.ErrorLog.Target)
	}
}

// --- Validation Tests ---

func TestLoadConfig_Validation_ServerConfig(t *testing.T) {
	tests := []struct {
		name        string
		configJSON  string
		expectError string
	}{
		{
			name:        "empty server address",
			configJSON:  `{"server": {"address": ""}}`,
			expectError: "server.address cannot be an empty string",
		},
		{
			name:        "empty executable_path if provided",
			configJSON:  `{"server": {"executable_path": ""}}`,
			expectError: "server.executable_path, if provided, cannot be empty",
		},
		{
			name:        "invalid child_readiness_timeout format",
			configJSON:  `{"server": {"child_readiness_timeout": "10"}}`, // No unit
			expectError: "invalid format for server.child_readiness_timeout '10': time: missing unit in duration \"10\"",
		},
		{
			name:        "non-positive child_readiness_timeout",
			configJSON:  `{"server": {"child_readiness_timeout": "0s"}}`,
			expectError: "server.child_readiness_timeout must be a positive duration, got '0s'",
		},
		{
			name:        "empty child_readiness_timeout if specified",
			configJSON:  `{"server": {"child_readiness_timeout": ""}}`,
			expectError: "server.child_readiness_timeout cannot be an empty string if specified",
		},
		{
			name:        "invalid graceful_shutdown_timeout format",
			configJSON:  `{"server": {"graceful_shutdown_timeout": "abc"}}`,
			expectError: "invalid format for server.graceful_shutdown_timeout 'abc': time: invalid duration \"abc\"",
		},
		{
			name:        "non-positive graceful_shutdown_timeout",
			configJSON:  `{"server": {"graceful_shutdown_timeout": "-5s"}}`,
			expectError: "server.graceful_shutdown_timeout must be a positive duration, got '-5s'",
		},
		{
			name:        "empty graceful_shutdown_timeout if specified",
			configJSON:  `{"server": {"graceful_shutdown_timeout": ""}}`,
			expectError: "server.graceful_shutdown_timeout cannot be an empty string if specified",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path, cleanup := writeTempFile(t, tc.configJSON, ".json")
			defer cleanup()
			_, err := LoadConfig(path)
			checkErrorContains(t, err, tc.expectError)
		})
	}
}

func TestLoadConfig_Validation_RoutingConfig(t *testing.T) {
	absPath := "/tmp" // Dummy absolute path for tests requiring it
	if _, err := os.Stat(absPath); os.IsNotExist(err) {
		if err := os.Mkdir(absPath, 0755); err != nil {
			t.Fatalf("Failed to create dummy dir %s: %v", absPath, err)
		}
		defer os.RemoveAll(absPath)
	}

	tests := []struct {
		name        string
		configJSON  string
		expectError string
	}{
		{
			name:        "empty path_pattern",
			configJSON:  `{"routing": {"routes": [{"path_pattern": "", "match_type": "Exact", "handler_type": "Test"}]}}`,
			expectError: "routing.routes[0].path_pattern cannot be empty",
		},
		{
			name:        "empty handler_type",
			configJSON:  `{"routing": {"routes": [{"path_pattern": "/test", "match_type": "Exact", "handler_type": ""}]}}`,
			expectError: "routing.routes[0].handler_type cannot be empty for path_pattern '/test'",
		},
		{
			name:        "exact match ends with / (not root)",
			configJSON:  `{"routing": {"routes": [{"path_pattern": "/admin/", "match_type": "Exact", "handler_type": "Test"}]}}`,
			expectError: "path_pattern '/admin/' with MatchType 'Exact' must not end with '/' unless it is the root path '/'",
		},
		{
			name:        "prefix match does not end with /",
			configJSON:  `{"routing": {"routes": [{"path_pattern": "/static", "match_type": "Prefix", "handler_type": "Test"}]}}`,
			expectError: "path_pattern '/static' with MatchType 'Prefix' must end with '/'",
		},
		{
			name:        "missing match_type",
			configJSON:  `{"routing": {"routes": [{"path_pattern": "/test", "handler_type": "Test"}]}}`,
			expectError: "routing.routes[0].match_type is missing for path_pattern '/test'; must be 'Exact' or 'Prefix'",
		},
		{
			name:        "invalid match_type",
			configJSON:  `{"routing": {"routes": [{"path_pattern": "/test", "match_type": "Invalid", "handler_type": "Test"}]}}`,
			expectError: "routing.routes[0].match_type 'Invalid' is invalid for path_pattern '/test'; must be 'Exact' or 'Prefix'",
		},
		{
			name: "ambiguous route exact",
			configJSON: `{"routing": {"routes": [
                {"path_pattern": "/test", "match_type": "Exact", "handler_type": "Test1"},
                {"path_pattern": "/test", "match_type": "Exact", "handler_type": "Test2"}
            ]}}`,
			expectError: "ambiguous route: duplicate PathPattern '/test' and MatchType 'Exact' found",
		},
		{
			name: "ambiguous route prefix",
			configJSON: `{"routing": {"routes": [
                {"path_pattern": "/test/", "match_type": "Prefix", "handler_type": "Test1"},
                {"path_pattern": "/test/", "match_type": "Prefix", "handler_type": "Test2"}
            ]}}`,
			expectError: "ambiguous route: duplicate PathPattern '/test/' and MatchType 'Prefix' found",
		},
		// StaticFileServer specific route validations
		{
			name:        "sfs missing handler_config",
			configJSON:  `{"routing": {"routes": [{"path_pattern": "/static/", "match_type": "Prefix", "handler_type": "StaticFileServer"}]}}`,
			expectError: "handler_config is missing for HandlerType 'StaticFileServer'",
		},
		{
			name:        "sfs empty handler_config",
			configJSON:  `{"routing": {"routes": [{"path_pattern": "/static/", "match_type": "Prefix", "handler_type": "StaticFileServer", "handler_config": {}}]}}`,
			expectError: "handler_config.document_root is required for HandlerType 'StaticFileServer'",
		},
		{
			name:        "sfs missing document_root",
			configJSON:  `{"routing": {"routes": [{"path_pattern": "/static/", "match_type": "Prefix", "handler_type": "StaticFileServer", "handler_config": {"index_files": ["index.html"]}}]}}`,
			expectError: "handler_config.document_root is required for HandlerType 'StaticFileServer'",
		},
		{
			name:        "sfs relative document_root",
			configJSON:  `{"routing": {"routes": [{"path_pattern": "/static/", "match_type": "Prefix", "handler_type": "StaticFileServer", "handler_config": {"document_root": "relative/path"}}]}}`,
			expectError: "handler_config.document_root 'relative/path' must be an absolute path",
		},
		{
			name:        "sfs mime_types_path and mime_types_map both specified",
			configJSON:  `{"routing": {"routes": [{"path_pattern": "/static/", "match_type": "Prefix", "handler_type": "StaticFileServer", "handler_config": {"document_root": "` + absPath + `", "mime_types_path": "mime.json", "mime_types_map": {".txt": "text/plain"}}}]}}`,
			expectError: "MimeTypesPath ('mime.json') and MimeTypesMap cannot both be specified",
		},
		{
			name:        "sfs empty mime_types_path if specified",
			configJSON:  `{"routing": {"routes": [{"path_pattern": "/static/", "match_type": "Prefix", "handler_type": "StaticFileServer", "handler_config": {"document_root": "` + absPath + `", "mime_types_path": ""}}]}}`,
			expectError: "handler_config.mime_types_path cannot be empty if specified",
		},
		{
			name:        "sfs mime_types_map key not starting with dot",
			configJSON:  `{"routing": {"routes": [{"path_pattern": "/static/", "match_type": "Prefix", "handler_type": "StaticFileServer", "handler_config": {"document_root": "` + absPath + `", "mime_types_map": {"txt": "text/plain"}}}]}}`,
			expectError: "mime_types_map key 'txt' must start with a '.'",
		},
		{
			name:        "sfs mime_types_map value empty",
			configJSON:  `{"routing": {"routes": [{"path_pattern": "/static/", "match_type": "Prefix", "handler_type": "StaticFileServer", "handler_config": {"document_root": "` + absPath + `", "mime_types_map": {".txt": ""}}}]}}`,
			expectError: "mime_types_map value for key '.txt' cannot be empty",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path, cleanup := writeTempFile(t, tc.configJSON, ".json")
			defer cleanup()
			_, err := LoadConfig(path)
			checkErrorContains(t, err, tc.expectError)
		})
	}
}

func TestLoadConfig_Validation_LoggingConfig(t *testing.T) {
	tests := []struct {
		name        string
		configJSON  string
		expectError string
	}{
		{
			name:        "invalid log_level",
			configJSON:  `{"logging": {"log_level": "TRACE"}}`,
			expectError: "logging.log_level 'TRACE' is invalid; must be one of 'DEBUG', 'INFO', 'WARNING', 'ERROR'",
		},
		// AccessLog validations
		{
			name:        "access_log empty target (should be caught by default)",
			configJSON:  `{"logging": {"access_log": {"target": ""}}}`,
			expectError: "logging.access_log.target cannot be empty",
		},
		{
			name:        "access_log relative file target",
			configJSON:  `{"logging": {"access_log": {"target": "logs/access.log"}}}`,
			expectError: "logging.access_log.target path 'logs/access.log' must be absolute",
		},
		{
			name:        "access_log invalid format",
			configJSON:  `{"logging": {"access_log": {"format": "clf"}}}`,
			expectError: "logging.access_log.format 'clf' is invalid; currently only 'json' is supported",
		},
		{
			name:        "access_log empty real_ip_header if provided",
			configJSON:  `{"logging": {"access_log": {"real_ip_header": ""}}}`,
			expectError: "logging.access_log.real_ip_header, if provided, cannot be empty",
		},
		{
			name:        "access_log invalid trusted_proxies cidr",
			configJSON:  `{"logging": {"access_log": {"trusted_proxies": ["192.168.1.0/33"]}}}`,
			expectError: "logging.access_log.trusted_proxies entry '192.168.1.0/33' is not a valid CIDR or IP address",
		},
		{
			name:        "access_log invalid trusted_proxies ip",
			configJSON:  `{"logging": {"access_log": {"trusted_proxies": ["not-an-ip"]}}}`,
			expectError: "logging.access_log.trusted_proxies entry 'not-an-ip' is not a valid CIDR or IP address",
		},
		// ErrorLog validations
		{
			name:        "error_log empty target (should be caught by default)",
			configJSON:  `{"logging": {"error_log": {"target": ""}}}`,
			expectError: "logging.error_log.target cannot be empty",
		},
		{
			name:        "error_log relative file target",
			configJSON:  `{"logging": {"error_log": {"target": "logs/error.log"}}}`,
			expectError: "logging.error_log.target path 'logs/error.log' must be absolute",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path, cleanup := writeTempFile(t, tc.configJSON, ".json")
			defer cleanup()
			_, err := LoadConfig(path)
			checkErrorContains(t, err, tc.expectError)
		})
	}
}

func TestParseAndValidateStaticFileServerConfig_MimeTypesFileInteraction(t *testing.T) {
	// Base directory for temporary files (main config, mime files)
	baseTempDir, err := ioutil.TempDir("", "configtest-main-")
	if err != nil {
		t.Fatalf("Failed to create base temp dir: %v", err)
	}
	defer os.RemoveAll(baseTempDir)

	// Create a dummy main config file path within baseTempDir. Its content doesn't matter, only its path for relative resolution.
	dummyMainConfigPath := filepath.Join(baseTempDir, "main_config.json")
	if err := ioutil.WriteFile(dummyMainConfigPath, []byte("{}"), 0644); err != nil {
		t.Fatalf("Failed to write dummy main config file: %v", err)
	}

	// Dummy absolute document root needed for SFS config.
	absDocRoot, err := ioutil.TempDir("", "sfs-docroot-")
	if err != nil {
		t.Fatalf("Failed to create temp docroot for SFS config: %v", err)
	}
	defer os.RemoveAll(absDocRoot)
	// Convert to forward slashes and quote for JSON embedding.
	// jsonSafeAbsDocRoot := strconv.Quote(filepath.ToSlash(absDocRoot)) // Not needed if using json.Marshal on map

	validMimeContent := ` { ".txt": "text/plain", ".custom": "application/x-custom" }`
	malformedMimeContent := `{not json`
	invalidKeyMimeContent := `{"txt": "text/plain"}` // Key "txt" should be ".txt"
	emptyValueMimeContent := `{".txt": ""}`

	tests := []struct {
		name                   string
		handlerConfigFields    map[string]interface{} // Fields to build handler_config JSON
		mimeFileName           string                 // Relative to baseTempDir (if relative path used in handlerConfig) or absolute.
		mimeFileIsAbsolute     bool                   // True if mimeFileName is an absolute path
		mimeFileContent        *string                // Content for the mime file. If nil, don't create. "DO_NOT_CREATE" to ensure absence.
		mainConfigPathForCall  string                 // Path to pass as mainConfigFilePath to ParseAndValidate...
		expectedErrorSubstring string
		expectedMimeCount      int
	}{
		{
			name: "valid_relative_mime_path",
			handlerConfigFields: map[string]interface{}{
				"document_root":   absDocRoot, // Will be quoted correctly by json.Marshal
				"mime_types_path": "rel_valid_mime.json",
			},
			mimeFileName:          "rel_valid_mime.json", // Relative to baseTempDir
			mimeFileContent:       &validMimeContent,
			mainConfigPathForCall: dummyMainConfigPath,
			expectedMimeCount:     2,
		},
		{
			name: "valid_absolute_mime_path",
			handlerConfigFields: map[string]interface{}{
				"document_root":   absDocRoot,
				"mime_types_path": filepath.Join(baseTempDir, "abs_valid_mime.json"), // Make it absolute
			},
			mimeFileName:          filepath.Join(baseTempDir, "abs_valid_mime.json"), // Actual absolute path for file creation
			mimeFileIsAbsolute:    true,
			mimeFileContent:       &validMimeContent,
			mainConfigPathForCall: dummyMainConfigPath, // mainConfigPath still needed even if mime path is abs
			expectedMimeCount:     2,
		},
		{
			name: "mime_path_file_not_found_relative",
			handlerConfigFields: map[string]interface{}{
				"document_root":   absDocRoot,
				"mime_types_path": "nonexistent_mime.json",
			},
			mimeFileName:           "nonexistent_mime.json",
			mimeFileContent:        strPtr("DO_NOT_CREATE"),
			mainConfigPathForCall:  dummyMainConfigPath,
			expectedErrorSubstring: "failed to read mime_types_path file",
		},
		{
			name: "mime_path_file_malformed_json_relative",
			handlerConfigFields: map[string]interface{}{
				"document_root":   absDocRoot,
				"mime_types_path": "malformed_mime.json",
			},
			mimeFileName:           "malformed_mime.json",
			mimeFileContent:        &malformedMimeContent,
			mainConfigPathForCall:  dummyMainConfigPath,
			expectedErrorSubstring: "failed to parse JSON from mime_types_path file",
		},
		{
			name: "mime_path_file_invalid_key_relative",
			handlerConfigFields: map[string]interface{}{
				"document_root":   absDocRoot,
				"mime_types_path": "invalidkey_mime.json",
			},
			mimeFileName:           "invalidkey_mime.json",
			mimeFileContent:        &invalidKeyMimeContent,
			mainConfigPathForCall:  dummyMainConfigPath,
			expectedErrorSubstring: "must start with a '.'",
		},
		{
			name: "mime_path_file_empty_value_relative",
			handlerConfigFields: map[string]interface{}{
				"document_root":   absDocRoot,
				"mime_types_path": "emptyval_mime.json",
			},
			mimeFileName:           "emptyval_mime.json",
			mimeFileContent:        &emptyValueMimeContent,
			mainConfigPathForCall:  dummyMainConfigPath,
			expectedErrorSubstring: "value for key \".txt\" cannot be empty",
		},
		{
			name: "mime_types_path_is_empty_string",
			handlerConfigFields: map[string]interface{}{
				"document_root":   absDocRoot,
				"mime_types_path": "",
			},
			mainConfigPathForCall:  dummyMainConfigPath,
			expectedErrorSubstring: "handler_config.mime_types_path cannot be empty if specified",
		},
		{
			name: "relative_mime_path_with_empty_mainConfigPath",
			handlerConfigFields: map[string]interface{}{
				"document_root":   absDocRoot,
				"mime_types_path": "some_rel_mime.json",
			},
			mimeFileName:           "some_rel_mime.json", // Won't be created as error is earlier
			mimeFileContent:        nil,                  // or DO_NOT_CREATE
			mainConfigPathForCall:  "",                   // Key part of this test
			expectedErrorSubstring: "cannot resolve relative mime_types_path \"some_rel_mime.json\": main configuration file path is not available",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Ensure DocumentRoot is in the map with the correct quoted path for JSON
			// Note: json.Marshal will handle quoting string values like absDocRoot,
			// so no need for jsonSafeAbsDocRoot directly in tc.handlerConfigFields map values.
			if _, ok := tc.handlerConfigFields["document_root"]; !ok {
				t.Fatal("Test case misconfiguration: document_root must be in handlerConfigFields")
			}

			rawHandlerConfig, err := json.Marshal(tc.handlerConfigFields)
			if err != nil {
				t.Fatalf("Failed to marshal handlerConfigFields to JSON: %v", err)
			}

			// Determine actual mime file path for creation/cleanup
			var actualMimeFilePath string
			if tc.mimeFileName != "" {
				if tc.mimeFileIsAbsolute {
					actualMimeFilePath = tc.mimeFileName // Already absolute
				} else {
					actualMimeFilePath = filepath.Join(baseTempDir, tc.mimeFileName)
				}
			}

			// Create or ensure absence of the mime file
			if actualMimeFilePath != "" {
				// Clean up any pre-existing file from previous sub-test run if names collide (unlikely with TempDir)
				_ = os.Remove(actualMimeFilePath)

				if tc.mimeFileContent != nil {
					if *tc.mimeFileContent == "DO_NOT_CREATE" {
						// Ensure it's gone
						_, statErr := os.Stat(actualMimeFilePath)
						if !os.IsNotExist(statErr) {
							t.Logf("File %s existed unexpectedly, removing.", actualMimeFilePath)
							_ = os.Remove(actualMimeFilePath)
						}
					} else {
						err := ioutil.WriteFile(actualMimeFilePath, []byte(*tc.mimeFileContent), 0644)
						if err != nil {
							t.Fatalf("Failed to write mime file %s: %v", actualMimeFilePath, err)
						}
						// No defer here, cleanup happens via baseTempDir or explicitly below if needed.
					}
				}
			}

			// DEBUG: Check if file exists immediately after writing
			_, statErr := os.Stat(actualMimeFilePath)
			if statErr == nil {
				t.Logf("Test %s: DEBUG - os.Stat check PASSED for %s immediately after write.", tc.name, actualMimeFilePath)
			} else {
				t.Logf("Test %s: DEBUG - os.Stat check FAILED for %s immediately after write: %v", tc.name, actualMimeFilePath, statErr)
			}
			sfsCfg, err := ParseAndValidateStaticFileServerConfig(rawHandlerConfig, tc.mainConfigPathForCall)

			if tc.expectedErrorSubstring != "" {
				checkErrorContains(t, err, tc.expectedErrorSubstring)
			} else {
				if err != nil {
					t.Fatalf("Expected no error, but got: %v", err)
				}
				if sfsCfg == nil {
					t.Fatal("Expected non-nil sfsCfg on success")
				}
				if sfsCfg.ResolvedMimeTypes == nil && tc.expectedMimeCount > 0 {
					t.Errorf("Expected ResolvedMimeTypes to be non-nil, got nil")
				}
				if len(sfsCfg.ResolvedMimeTypes) != tc.expectedMimeCount {
					t.Errorf("Expected %d resolved mime types, got %d (%v)", tc.expectedMimeCount, len(sfsCfg.ResolvedMimeTypes), sfsCfg.ResolvedMimeTypes)
				}
				if tc.expectedMimeCount > 0 { // Check specific entries if expected
					if tc.mimeFileContent != nil && *tc.mimeFileContent == validMimeContent {
						if sfsCfg.ResolvedMimeTypes[".txt"] != "text/plain" {
							t.Errorf("Expected .txt -> text/plain, got %s", sfsCfg.ResolvedMimeTypes[".txt"])
						}
						if sfsCfg.ResolvedMimeTypes[".custom"] != "application/x-custom" {
							t.Errorf("Expected .custom -> application/x-custom, got %s", sfsCfg.ResolvedMimeTypes[".custom"])
						}
					}
				}
			}

			// Clean up the specific mime file if it was created
			if actualMimeFilePath != "" && tc.mimeFileContent != nil && *tc.mimeFileContent != "DO_NOT_CREATE" {
				os.Remove(actualMimeFilePath)
			}
		})
	}
}
func TestParseAndValidateStaticFileServerConfig_Defaults(t *testing.T) {
	docRoot, _ := ioutil.TempDir("", "docroot")
	defer os.RemoveAll(docRoot)

	rawCfg := []byte(`{"document_root": "` + docRoot + `"}`)
	sfsCfg, err := ParseAndValidateStaticFileServerConfig(rawCfg, "")
	if err != nil {
		t.Fatalf("ParseAndValidateStaticFileServerConfig failed: %v", err)
	}

	if len(sfsCfg.IndexFiles) != 1 || sfsCfg.IndexFiles[0] != "index.html" {
		t.Errorf("Expected default IndexFiles to be [\"index.html\"], got %v", sfsCfg.IndexFiles)
	}
	if sfsCfg.ServeDirectoryListing == nil || *sfsCfg.ServeDirectoryListing != false {
		t.Errorf("Expected default ServeDirectoryListing to be false, got %v", sfsCfg.ServeDirectoryListing)
	}
}

func TestParseAndValidateStaticFileServerConfig_Validations(t *testing.T) {
	docRoot, _ := ioutil.TempDir("", "docroot")
	defer os.RemoveAll(docRoot)

	tests := []struct {
		name        string
		rawConfig   string
		expectError string
	}{
		{"nil config", "null", "handler_config for StaticFileServer cannot be empty or null; document_root is required"},
		{"empty config", "{}", "handler_config for StaticFileServer cannot be empty or null; document_root is required"}, // Adjusted message
		{"missing document_root", `{"index_files": ["test.html"]}`, "handler_config.document_root is required for StaticFileServer"},
		{"relative document_root", `{"document_root": "nodir"}`, `handler_config.document_root "nodir" must be an absolute path`},
		{"mime_types_path and mime_types_map", `{"document_root": "` + docRoot + `", "mime_types_path": "file.json", "mime_types_map": {".x": "y"}}`, `MimeTypesPath ("file.json") and MimeTypesMap cannot both be specified`},
		{"mime_types_map key no dot", `{"document_root": "` + docRoot + `", "mime_types_map": {"x": "y"}}`, `mime_types_map key "x" must start with a '.'`},
		{"mime_types_map value empty", `{"document_root": "` + docRoot + `", "mime_types_map": {".x": ""}}`, `mime_types_map value for key ".x" cannot be empty`},
		{"index_files empty string", `{"document_root": "` + docRoot + `", "index_files": [""]}`, "handler_config.index_files[0] cannot be an empty string"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseAndValidateStaticFileServerConfig([]byte(tc.rawConfig), "")
			checkErrorContains(t, err, tc.expectError)
		})
	}
}
func TestStaticFileServer_DefaultAppliedInGlobalConfig(t *testing.T) {
	absDocRoot, err := ioutil.TempDir("", "testdocs-")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(absDocRoot)

	configContent := `{
		"routing": {
			"routes": [
				{
					"path_pattern": "/files/",
					"match_type": "Prefix",
					"handler_type": "StaticFileServer",
					"handler_config": {
						"document_root": "` + absDocRoot + `"
					}
				}
			]
		}
	}`

	path, cleanup := writeTempFile(t, configContent, ".json")
	defer cleanup()

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}

	if len(cfg.Routing.Routes) != 1 {
		t.Fatalf("Expected 1 route, got %d", len(cfg.Routing.Routes))
	}

	route := cfg.Routing.Routes[0]
	if route.HandlerType != "StaticFileServer" {
		t.Fatalf("Expected HandlerType StaticFileServer, got %s", route.HandlerType)
	}

	// The defaults for StaticFileServerConfig are applied by ParseAndValidateStaticFileServerConfig.
	// LoadConfig itself doesn't dive into handler_config to apply these specific defaults;
	// it only applies global defaults. The validation step (or handler instantiation)
	// is responsible for parsing and validating specific handler configs.
	// This test ensures that if we *do* parse it using the correct function, defaults are applied.

	parsedSfsCfg, err := ParseAndValidateStaticFileServerConfig(route.HandlerConfig, cfg.OriginalFilePath())
	if err != nil {
		t.Fatalf("ParseAndValidateStaticFileServerConfig failed: %v", err)
	}

	// Check defaults applied by ParseAndValidateStaticFileServerConfig
	if len(parsedSfsCfg.IndexFiles) != 1 || parsedSfsCfg.IndexFiles[0] != "index.html" {
		t.Errorf("Expected default IndexFiles [\"index.html\"], got %v", parsedSfsCfg.IndexFiles)
	}
	if parsedSfsCfg.ServeDirectoryListing == nil || *parsedSfsCfg.ServeDirectoryListing != false {
		t.Errorf("Expected default ServeDirectoryListing to be false, got %v", parsedSfsCfg.ServeDirectoryListing)
	}
}

func TestDuration_Unmarshal(t *testing.T) {
	tests := []struct {
		name      string
		inputJSON string // JSON representation of the field containing the duration
		inputTOML string // TOML representation of the field containing the duration
		fieldName string
		expectErr string
		expectDur time.Duration
	}{
		{
			name:      "valid duration json",
			inputJSON: `{"timeout": "10s"}`,
			fieldName: "timeout",
			expectDur: 10 * time.Second,
		},
		{
			name:      "valid duration toml",
			inputTOML: `timeout = "15m"`,
			fieldName: "timeout",
			expectDur: 15 * time.Minute,
		},
		{
			name:      "invalid duration string json",
			inputJSON: `{"timeout": "10"}`, // missing unit
			fieldName: "timeout",
			expectErr: "invalid duration string \"10\": time: missing unit in duration",
		},
		{
			name:      "invalid duration string toml",
			inputTOML: `timeout = "abc"`, // invalid
			fieldName: "timeout",
			expectErr: "invalid duration string \"abc\": time: invalid duration",
		},
		{
			name:      "non-positive duration json",
			inputJSON: `{"timeout": "0s"}`,
			fieldName: "timeout",
			expectErr: "duration must be positive, got \"0s\"",
		},
		{
			name:      "non-positive duration toml",
			inputTOML: `timeout = "-1h"`,
			fieldName: "timeout",
			expectErr: "duration must be positive, got \"-1h\"",
		},
		{
			name:      "not a string json",
			inputJSON: `{"timeout": 10}`,
			fieldName: "timeout",
			expectErr: "duration should be a string, got 10",
		},
		{
			name:      "empty string json",
			inputJSON: `{"timeout": ""}`,
			fieldName: "timeout",
			expectErr: "duration string cannot be empty",
		},
		{
			name:      "empty string toml",
			inputTOML: `timeout = ""`,
			fieldName: "timeout",
			expectErr: "duration string cannot be empty",
		},
	}

	type TestStructJSON struct {
		Timeout Duration `json:"timeout"`
	}
	type TestStructTOML struct {
		Timeout Duration `toml:"timeout"`
	}

	for _, tc := range tests {
		t.Run(tc.name+"_json", func(t *testing.T) {
			if tc.inputJSON == "" {
				t.Skip("No JSON input for this test case")
			}
			var s TestStructJSON
			err := json.Unmarshal([]byte(tc.inputJSON), &s)
			if tc.expectErr != "" {
				checkErrorContains(t, err, tc.expectErr)
			} else {
				if err != nil {
					t.Fatalf("Expected no error, got %v", err)
				}
				if s.Timeout.Value() != tc.expectDur {
					t.Errorf("Expected duration %v, got %v", tc.expectDur, s.Timeout.Value())
				}
				if s.Timeout.String() != tc.expectDur.String() {
					t.Errorf("Expected duration string %v, got %v", tc.expectDur.String(), s.Timeout.String())
				}
			}
		})
		t.Run(tc.name+"_toml", func(t *testing.T) {
			if tc.inputTOML == "" {
				t.Skip("No TOML input for this test case")
			}
			var s TestStructTOML
			err := toml.Unmarshal([]byte(tc.inputTOML), &s)
			if tc.expectErr != "" {
				checkErrorContains(t, err, tc.expectErr)
			} else {
				if err != nil {
					t.Fatalf("Expected no error, got %v", err)
				}
				if s.Timeout.Value() != tc.expectDur {
					t.Errorf("Expected duration %v, got %v", tc.expectDur, s.Timeout.Value())
				}
			}
		})
	}
}

func TestLoadConfig_OriginalFilePath(t *testing.T) {
	content := `{"server": {"address": ":8080"}}`
	path, cleanup := writeTempFile(t, content, ".json")
	defer cleanup()

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}
	if cfg == nil {
		t.Fatal("Expected config to be non-nil")
	}

	if cfg.OriginalFilePath() != path {
		t.Errorf("Expected OriginalFilePath() to be %q, got %q", path, cfg.OriginalFilePath())
	}

	// Test with a nil config
	var nilCfg *Config
	if nilCfg.OriginalFilePath() != "" {
		t.Errorf("Expected OriginalFilePath() on nil config to be \"\", got %q", nilCfg.OriginalFilePath())
	}
}

func TestDuration_DirectUnmarshalMethods(t *testing.T) {
	t.Run("UnmarshalText", func(t *testing.T) {
		tests := []struct {
			name      string
			input     string
			expectErr string
			expectDur time.Duration
		}{
			{"valid", "30s", "", 30 * time.Second},
			{"valid with minutes", "2m", "", 2 * time.Minute},
			{"invalid format", "10", "invalid duration string \"10\": time: missing unit in duration", 0},
			{"invalid chars", "abc", "invalid duration string \"abc\": time: invalid duration", 0},
			{"non-positive zero", "0s", "duration must be positive, got \"0s\"", 0},
			{"non-positive negative", "-5m", "duration must be positive, got \"-5m\"", 0},
			{"empty string", "", "duration string cannot be empty", 0},
		}

		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				var d Duration
				err := d.UnmarshalText([]byte(tc.input))

				if tc.expectErr != "" {
					checkErrorContains(t, err, tc.expectErr)
				} else {
					if err != nil {
						t.Fatalf("UnmarshalText(%q) unexpected error: %v", tc.input, err)
					}
					if d.Value() != tc.expectDur {
						t.Errorf("UnmarshalText(%q) expected duration %v, got %v", tc.input, tc.expectDur, d.Value())
					}
				}
			})
		}
	})

	t.Run("UnmarshalJSON", func(t *testing.T) {
		tests := []struct {
			name      string
			inputJSON string // Raw JSON bytes
			expectErr string
			expectDur time.Duration
		}{
			{"valid string", `"45s"`, "", 45 * time.Second},
			{"valid string with hours", `"1h"`, "", 1 * time.Hour},
			{"invalid format in string", `"20"`, "invalid duration string \"20\": time: missing unit in duration", 0},
			{"invalid chars in string", `"xyz"`, "invalid duration string \"xyz\": time: invalid duration", 0},
			{"non-positive zero in string", `"0s"`, "duration must be positive, got \"0s\"", 0},
			{"non-positive negative in string", `"-30m"`, "duration must be positive, got \"-30m\"", 0},
			{"empty string literal", `""`, "duration string cannot be empty", 0},
			{"incorrect type (number)", `123`, "duration should be a string, got 123", 0},
			{"incorrect type (boolean)", `true`, "duration should be a string, got true", 0},
			{"incorrect type (null)", `null`, "duration string cannot be empty", 0}, // json.Unmarshal of "null" into string results in empty string, then UnmarshalText fails.
		}

		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				var d Duration
				err := d.UnmarshalJSON([]byte(tc.inputJSON))

				if tc.expectErr != "" {
					checkErrorContains(t, err, tc.expectErr)
				} else {
					if err != nil {
						t.Fatalf("UnmarshalJSON(%s) unexpected error: %v", tc.inputJSON, err)
					}
					if d.Value() != tc.expectDur {
						t.Errorf("UnmarshalJSON(%s) expected duration %v, got %v", tc.inputJSON, tc.expectDur, d.Value())
					}
				}
			})
		}
	})
}

func TestIsFilePath(t *testing.T) {
	tests := []struct {
		name     string
		target   string
		expected bool
	}{
		{"stdout", "stdout", false},
		{"stderr", "stderr", false},
		{"absolute path unix", "/var/log/app.log", true},
		{"absolute path windows", "C:\\logs\\app.log", true},
		{"relative path", "logs/app.log", true},
		{"simple file name", "app.log", true},
		{"empty string", "", true}, // IsFilePath considers empty string a path (though validation would reject it)
		{"path with spaces", "/my logs/app.log", true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			actual := IsFilePath(tc.target)
			if actual != tc.expected {
				t.Errorf("IsFilePath(%q) = %v; want %v", tc.target, actual, tc.expected)
			}
		})
	}
}
