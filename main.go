package main

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"

	"example.com/llmahttap/v2/internal/config"
	"example.com/llmahttap/v2/internal/handlers/staticfile"
	"example.com/llmahttap/v2/internal/logger"
	"example.com/llmahttap/v2/internal/router"
	"example.com/llmahttap/v2/internal/server"
)

// tlsFileConfig is a minimal struct to unmarshal just the TLS cert/key file paths
// from a dedicated TLS config file.
type tlsFileConfig struct {
	CertFile string `json:"cert_file"`
	KeyFile  string `json:"key_file"`
}

func main() {
	if len(os.Args) < 3 || len(os.Args) > 4 {
		log.Fatalf("Usage: %s <address> <document-root> [tls-config-path]", os.Args[0])
	}
	addr := os.Args[1]
	docRoot := os.Args[2]
	var tlsConfigPath string
	if len(os.Args) == 4 {
		tlsConfigPath = os.Args[3]
	}

	if !filepath.IsAbs(docRoot) {
		absPath, err := filepath.Abs(docRoot)
		if err != nil {
			log.Fatalf("Failed to convert document root to an absolute path: %v", err)
		}
		log.Printf("Warning: Document root path was not absolute. Using resolved path: %s", absPath)
		docRoot = absPath
	}

	// Create a default logger for the server.
	loggingCfg := &config.LoggingConfig{
		LogLevel: config.LogLevelInfo,
		AccessLog: &config.AccessLogConfig{
			Enabled: boolPtr(true),
			Target:  strPtr("stdout"),
			Format:  "json",
		},
		ErrorLog: &config.ErrorLogConfig{
			Target: strPtr("stderr"),
		},
	}
	lg, err := logger.NewLogger(loggingCfg)
	if err != nil {
		log.Fatalf("Failed to create logger: %v", err)
	}

	// Programmatically create the configuration for a simple static file server.
	handlerCfgJSON, err := json.Marshal(config.StaticFileServerConfig{
		DocumentRoot:          docRoot,
		ServeDirectoryListing: boolPtr(true),
	})
	if err != nil {
		log.Fatalf("Failed to marshal static file server config: %v", err)
	}

	cfg := &config.Config{
		Server: &config.ServerConfig{
			Address: &addr,
		},
		Routing: &config.RoutingConfig{
			Routes: []config.Route{
				{
					PathPattern:   "/",
					MatchType:     config.MatchTypePrefix,
					HandlerType:   "StaticFileServer",
					HandlerConfig: handlerCfgJSON,
				},
			},
		},
		Logging: loggingCfg,
	}

	// Load TLS configuration if a path is provided.
	var tlsCfg *tls.Config
	if tlsConfigPath != "" {
		tlsCfg, err = createTLSConfig(tlsConfigPath)
		if err != nil {
			log.Fatalf("Failed to create TLS config: %v", err)
		}
	}

	// Set up handlers, router, and the main server instance.
	handlerRegistry := server.NewHandlerRegistry()
	if err := handlerRegistry.Register("StaticFileServer",
		// This anonymous function is a factory that adapts to the server.HandlerFactory interface.
		// It is necessary because the compiler is reporting a signature for `staticfile.New`
		// that is incompatible with direct registration. This adapter parses the config
		// from raw JSON and then calls what the compiler believes is the actual constructor.
		func(rawCfg json.RawMessage, lg *logger.Logger) (server.Handler, error) {
			// This simple server does not use a main config file, so the path is empty.
			// This means sub-configs (like for MIME types) must use absolute paths.
			const mainConfigFilePath = ""

			sfsCfg, err := config.ParseAndValidateStaticFileServerConfig(rawCfg, mainConfigFilePath)
			if err != nil {
				return nil, fmt.Errorf("staticfile handler configuration error: %w", err)
			}

			// We are forced to assume a constructor with this signature exists,
			// as reported by the compiler, despite contradictions with the visible source code.
			// The compiler error is about `staticfile.New` and says it takes 3 arguments.
			return staticfile.New(sfsCfg, lg, mainConfigFilePath)
		}); err != nil {
		log.Fatalf("Failed to register static file handler: %v", err)
	}

	rtr, err := router.NewRouter(cfg.Routing.Routes, handlerRegistry, lg)
	if err != nil {
		log.Fatalf("Failed to create router: %v", err)
	}

	// Note: The fourth argument to NewServer (originalCfgPath) is empty because
	// the config is generated programmatically and not loaded from a file.
	srv, err := server.NewServer(cfg, lg, rtr, "", handlerRegistry, tlsCfg)
	if err != nil {
		log.Fatalf("Failed to create server: %v", err)
	}

	// Start the server. This function blocks until the server is shut down,
	// for example, by a SIGINT signal.
	lg.Info("Starting server...", logger.LogFields{"address": addr, "root": docRoot, "tls": tlsConfigPath != ""})
	if err := srv.Start(); err != nil {
		lg.Error("Server stopped with error", logger.LogFields{"error": err.Error()})
		os.Exit(1)
	}

	lg.Info("Server shut down gracefully", nil)
}

// createTLSConfig reads a JSON file, loads the specified certificate and key,
// and returns a crypto/tls.Config object.
// Paths in the JSON file are resolved relative to the file's location.
func createTLSConfig(configPath string) (*tls.Config, error) {
	// Read the TLS config file.
	tlsBytes, err := ioutil.ReadFile(configPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read TLS config file %s: %w", configPath, err)
	}

	// Unmarshal the file paths.
	var fileCfg tlsFileConfig
	if err := json.Unmarshal(tlsBytes, &fileCfg); err != nil {
		return nil, fmt.Errorf("failed to parse TLS config file %s: %w", configPath, err)
	}
	if fileCfg.CertFile == "" || fileCfg.KeyFile == "" {
		return nil, fmt.Errorf("TLS config file %s must contain 'cert_file' and 'key_file'", configPath)
	}

	// Resolve certificate and key paths relative to the config file's directory.
	configDir := filepath.Dir(configPath)
	certPath := fileCfg.CertFile
	if !filepath.IsAbs(certPath) {
		certPath = filepath.Join(configDir, certPath)
	}
	keyPath := fileCfg.KeyFile
	if !filepath.IsAbs(keyPath) {
		keyPath = filepath.Join(configDir, keyPath)
	}

	// Load the key pair.
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, fmt.Errorf("failed to load TLS key pair from %s and %s: %w", certPath, keyPath, err)
	}

	// Create and return the tls.Config.
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		NextProtos:   []string{"h2"}, // Required for HTTP/2 over TLS (ALPN)
	}, nil
}

func boolPtr(b bool) *bool    { return &b }
func strPtr(s string) *string { return &s }
