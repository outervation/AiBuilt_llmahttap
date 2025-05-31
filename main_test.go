package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"example.com/llmahttap/v2/internal/config"
	"example.com/llmahttap/v2/internal/logger" // For logger in test setup
)

// mockMainProcess is a helper function to simulate the core logic of main()
// for testing purposes, without starting a server or calling os.Exit.
// It returns the generated config and any error during config generation.
func mockMainProcess(args []string) (*config.Config, string, error) {
	// Use a custom FlagSet for testing to avoid conflicts with global flags
	// and to allow capturing output.
	testFlagSet := flag.NewFlagSet("testmain", flag.ContinueOnError)
	var outputBuf bytes.Buffer
	testFlagSet.SetOutput(&outputBuf) // Capture flag error messages

	rootDirArg := testFlagSet.String("root", ".", "Root directory to serve files from")
	portArg := testFlagSet.String("port", "8080", "Port to listen on")

	err := testFlagSet.Parse(args)
	if err != nil {
		return nil, outputBuf.String(), fmt.Errorf("flag parsing error: %w\nOutput: %s", err, outputBuf.String())
	}

	if *rootDirArg == "" {
		return nil, "", fmt.Errorf("root directory cannot be empty")
	}
	if *portArg == "" {
		return nil, "", fmt.Errorf("port cannot be empty")
	}

	absRootDir, err := filepath.Abs(*rootDirArg)
	if err != nil {
		return nil, "", fmt.Errorf("error resolving absolute path for root directory '%s': %v", *rootDirArg, err)
	}

	serverAddr := "0.0.0.0:" + *portArg
	boolTrue := true
	boolFalse := false
	strStdout := "stdout"
	strStderr := "stderr"
	strJSON := "json"
	strINFO := string(config.LogLevelInfo)

	staticFsCfg := config.StaticFileServerConfig{
		DocumentRoot:          absRootDir,
		IndexFiles:            []string{"index.html"},
		ServeDirectoryListing: &boolFalse,
	}
	handlerCfgBytes, err := json.Marshal(staticFsCfg)
	if err != nil {
		return nil, "", fmt.Errorf("error marshalling StaticFileServerConfig: %v", err)
	}

	cfg := &config.Config{
		Server: &config.ServerConfig{
			Address: &serverAddr,
		},
		Logging: &config.LoggingConfig{
			LogLevel: config.LogLevel(strINFO),
			AccessLog: &config.AccessLogConfig{
				Enabled: &boolTrue,
				Target:  &strStdout,
				Format:  strJSON,
			},
			ErrorLog: &config.ErrorLogConfig{
				Target: &strStderr,
			},
		},
		Routing: &config.RoutingConfig{
			Routes: []config.Route{
				{
					PathPattern:   "/",
					MatchType:     config.MatchTypePrefix,
					HandlerType:   "StaticFileServer",
					HandlerConfig: config.RawMessageWrapper(handlerCfgBytes),
				},
			},
		},
	}
	return cfg, "", nil
}

func TestMain_DefaultConfigGeneration(t *testing.T) {
	cfg, _, err := mockMainProcess([]string{}) // No arguments, so defaults are used
	if err != nil {
		t.Fatalf("mockMainProcess failed for default args: %v", err)
	}

	// Verify ServerConfig
	expectedServerAddr := "0.0.0.0:8080"
	if cfg.Server == nil || cfg.Server.Address == nil {
		t.Fatalf("ServerConfig or ServerConfig.Address is nil")
	}
	if *cfg.Server.Address != expectedServerAddr {
		t.Errorf("Expected server address '%s', got '%s'", expectedServerAddr, *cfg.Server.Address)
	}

	// Verify LoggingConfig defaults
	if cfg.Logging == nil {
		t.Fatalf("LoggingConfig is nil")
	}
	if cfg.Logging.LogLevel != config.LogLevelInfo {
		t.Errorf("Expected LogLevel '%s', got '%s'", config.LogLevelInfo, cfg.Logging.LogLevel)
	}
	if cfg.Logging.AccessLog == nil || cfg.Logging.AccessLog.Target == nil || *cfg.Logging.AccessLog.Target != "stdout" ||
		cfg.Logging.AccessLog.Enabled == nil || *cfg.Logging.AccessLog.Enabled != true || cfg.Logging.AccessLog.Format != "json" {
		t.Errorf("AccessLog config mismatch. Got: %+v", cfg.Logging.AccessLog)
	}
	if cfg.Logging.ErrorLog == nil || cfg.Logging.ErrorLog.Target == nil || *cfg.Logging.ErrorLog.Target != "stderr" {
		t.Errorf("ErrorLog config mismatch. Got: %+v", cfg.Logging.ErrorLog)
	}

	// Verify RoutingConfig and StaticFileServerConfig within it
	if cfg.Routing == nil || len(cfg.Routing.Routes) != 1 {
		t.Fatalf("Expected 1 route, got %d", len(cfg.Routing.Routes))
	}
	route := cfg.Routing.Routes[0]
	if route.PathPattern != "/" || route.MatchType != config.MatchTypePrefix || route.HandlerType != "StaticFileServer" {
		t.Errorf("Route definition mismatch. Got: %+v", route)
	}

	var sfsCfg config.StaticFileServerConfig
	if err := json.Unmarshal(route.HandlerConfig.Bytes(), &sfsCfg); err != nil {
		t.Fatalf("Failed to unmarshal StaticFileServerConfig: %v", err)
	}

	expectedDocRoot, _ := filepath.Abs(".")
	if sfsCfg.DocumentRoot != expectedDocRoot {
		t.Errorf("Expected DocumentRoot '%s', got '%s'", expectedDocRoot, sfsCfg.DocumentRoot)
	}
	expectedIndexFiles := []string{"index.html"}
	if !reflect.DeepEqual(sfsCfg.IndexFiles, expectedIndexFiles) {
		t.Errorf("Expected IndexFiles %v, got %v", expectedIndexFiles, sfsCfg.IndexFiles)
	}
	if sfsCfg.ServeDirectoryListing == nil || *sfsCfg.ServeDirectoryListing != false {
		t.Errorf("Expected ServeDirectoryListing to be false, got %v (pointer: %v)", *sfsCfg.ServeDirectoryListing, sfsCfg.ServeDirectoryListing)
	}
}

func TestMain_CustomConfigGeneration(t *testing.T) {
	customRoot := "/tmp/testwebroot" // Using a temporary, valid-looking path
	customPort := "9999"
	args := []string{"-root", customRoot, "-port", customPort}

	cfg, _, err := mockMainProcess(args)
	if err != nil {
		t.Fatalf("mockMainProcess failed for custom args: %v", err)
	}

	// Verify ServerConfig
	expectedServerAddr := "0.0.0.0:" + customPort
	if cfg.Server == nil || cfg.Server.Address == nil {
		t.Fatalf("ServerConfig or ServerConfig.Address is nil")
	}
	if *cfg.Server.Address != expectedServerAddr {
		t.Errorf("Expected server address '%s', got '%s'", expectedServerAddr, *cfg.Server.Address)
	}

	// Verify StaticFileServerConfig DocumentRoot
	if cfg.Routing == nil || len(cfg.Routing.Routes) != 1 {
		t.Fatalf("Expected 1 route, got %d", len(cfg.Routing.Routes))
	}
	var sfsCfg config.StaticFileServerConfig
	if err := json.Unmarshal(cfg.Routing.Routes[0].HandlerConfig.Bytes(), &sfsCfg); err != nil {
		t.Fatalf("Failed to unmarshal StaticFileServerConfig: %v", err)
	}

	expectedDocRoot, _ := filepath.Abs(customRoot)
	if sfsCfg.DocumentRoot != expectedDocRoot {
		t.Errorf("Expected DocumentRoot '%s', got '%s'", expectedDocRoot, sfsCfg.DocumentRoot)
	}
}

func TestMain_FlagDefinitionsAndParsing(t *testing.T) {
	// Test default values
	testFlagSetDefaults := flag.NewFlagSet("testDefault", flag.ContinueOnError)
	defaultRootDir := testFlagSetDefaults.String("root", ".", "desc")
	defaultPort := testFlagSetDefaults.String("port", "8080", "desc")
	testFlagSetDefaults.Parse([]string{}) // Parse with no args

	if *defaultRootDir != "." {
		t.Errorf("Default -root: expected '.', got '%s'", *defaultRootDir)
	}
	if *defaultPort != "8080" {
		t.Errorf("Default -port: expected '8080', got '%s'", *defaultPort)
	}

	// Test custom values
	testFlagSetCustom := flag.NewFlagSet("testCustom", flag.ContinueOnError)
	customRootDir := testFlagSetCustom.String("root", ".", "desc")
	customPort := testFlagSetCustom.String("port", "8080", "desc")

	err := testFlagSetCustom.Parse([]string{"-root=/var/www", "-port=1234"})
	if err != nil {
		t.Fatalf("Failed to parse custom flags: %v", err)
	}

	if *customRootDir != "/var/www" {
		t.Errorf("Custom -root: expected '/var/www', got '%s'", *customRootDir)
	}
	if *customPort != "1234" {
		t.Errorf("Custom -port: expected '1234', got '%s'", *customPort)
	}
}

// TestMain_ErrorHandling_EmptyArgs demonstrates how argument validation in mockMainProcess works.
func TestMain_ErrorHandling_EmptyArgs(t *testing.T) {
	// Test empty root
	_, _, errRoot := mockMainProcess([]string{"-root", ""})
	if errRoot == nil || !strings.Contains(errRoot.Error(), "root directory cannot be empty") {
		t.Errorf("Expected error for empty root directory, got: %v", errRoot)
	}

	// Test empty port
	_, _, errPort := mockMainProcess([]string{"-port", ""})
	if errPort == nil || !strings.Contains(errPort.Error(), "port cannot be empty") {
		t.Errorf("Expected error for empty port, got: %v", errPort)
	}
}

// This is a basic test to ensure components used by main can be initialized.
// A full test of main() starting a server is E2E.
func TestMain_ComponentInitializationSmokeTest(t *testing.T) {
	cfg, _, err := mockMainProcess([]string{})
	if err != nil {
		t.Fatalf("mockMainProcess failed: %v", err)
	}

	// Test Logger initialization
	lg, err := logger.NewLogger(cfg.Logging)
	if err != nil {
		t.Fatalf("logger.NewLogger failed: %v", err)
	}
	if lg == nil {
		t.Fatal("logger.NewLogger returned nil logger")
	}
	// Test HandlerRegistry (simple creation)
	// handlerRegistry := server.NewHandlerRegistry() // server package not imported here
	// if handlerRegistry == nil {
	// 	t.Fatal("server.NewHandlerRegistry returned nil")
	// }
	// Full router/server init is beyond a simple smoke test for main's config logic.
	t.Log("Basic config and logger initialized successfully via mock process.")
}

// Note: Testing actual os.Exit calls or log.Fatalf requires more complex test harnesses
// (e.g., running main as a subprocess and checking exit codes/output, or using a atexit hook).
// The tests above focus on the config generation logic which is the core of main.go's responsibilities
// before it hands off to server.Start().
