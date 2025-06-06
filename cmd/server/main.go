package main

import (
	"crypto/tls" // Added for TLS configuration
	"encoding/json"
	"flag"
	"fmt"
	"log" // Added
	"os"
	"path/filepath" // Added
	"time"          // Added
	// "os/signal" // Removed, unused in current main body
	// "syscall"   // Removed, unused in current main body

	"example.com/llmahttap/v2/internal/config"
	"example.com/llmahttap/v2/internal/handlers/staticfile"
	"example.com/llmahttap/v2/internal/logger"
	"example.com/llmahttap/v2/internal/router"
	"example.com/llmahttap/v2/internal/server"
)

var (
	configFilePath string
)

func setupAndRunServer(cfg *config.Config, originalCfgPath string) error {
	// 2. Initialize Logger
	appLogger, err := logger.NewLogger(cfg.Logging)
	if err != nil {
		// This error is critical for server operation, log it directly if possible,
		// but it will be returned and handled by the caller (main).
		// Using fmt.Fprintf for initial startup errors if logger itself fails.
		fmt.Fprintf(os.Stderr, "Failed to initialize logger: %v\n", err)
		return fmt.Errorf("failed to initialize logger: %w", err)
	}
	defer func() {
		if err := appLogger.CloseLogFiles(); err != nil {
			// Use standard log for this fallback, as custom logger might be compromised
			log.Printf("Error closing log files during shutdown: %v", err)
		}
	}()
	appLogger.Info("Logger initialized", nil)

	// 3. Initialize Handler Registry and register handlers
	handlerRegistry := server.NewHandlerRegistry()

	// Register StaticFileServer Handler Factory
	// The factory function is a closure, capturing 'originalCfgPath' from setupAndRunServer's arguments.
	staticFileServerFactory := func(handlerConfig json.RawMessage, factoryLogger *logger.Logger) (server.Handler, error) {
		// 'originalCfgPath' is the absolute path to the main config file.
		// It's used by ParseAndValidateStaticFileServerConfig to resolve relative paths within the static server's config.
		staticServerSpecificConfig, err := config.ParseAndValidateStaticFileServerConfig(handlerConfig, originalCfgPath)
		if err != nil {
			return nil, fmt.Errorf("StaticFileServer: failed to parse/validate specific handler config: %w", err)
		}

		handler, err := staticfile.New(staticServerSpecificConfig, factoryLogger, originalCfgPath)
		if err != nil {
			return nil, fmt.Errorf("StaticFileServer: failed to create handler instance: %w", err)
		}
		return handler, nil
	}

	if err := handlerRegistry.Register("StaticFileServer", staticFileServerFactory); err != nil {
		appLogger.Error("Failed to register StaticFileServer handler factory", logger.LogFields{"error": err.Error()})
		return fmt.Errorf("failed to register StaticFileServer handler factory: %w", err)
	}
	appLogger.Info("Registered StaticFileServer handler factory.", nil)

	if handlerRegistry == nil {
		appLogger.Error("Handler registry is nil after initialization (should not happen)", nil)
		return fmt.Errorf("handler registry became nil after initialization")
	}
	appLogger.Info("Handler registry initialized.", nil)

	// 4. Initialize Router
	var routesToUse []config.Route
	if cfg.Routing != nil {
		routesToUse = cfg.Routing.Routes
	}
	appRouter, err := router.NewRouter(routesToUse, handlerRegistry, appLogger)
	if err != nil {
		appLogger.Error("Failed to initialize router", logger.LogFields{"error": err.Error()})
		return fmt.Errorf("failed to initialize router: %w", err)
	}
	appLogger.Info("Router initialized", nil)

	// 5. Initialize Server
	var tlsConfiguration *tls.Config
	if cfg.Server != nil && cfg.Server.TLS != nil && cfg.Server.TLS.Enabled != nil && *cfg.Server.TLS.Enabled {
		if cfg.Server.TLS.CertFile == nil || *cfg.Server.TLS.CertFile == "" ||
			cfg.Server.TLS.KeyFile == nil || *cfg.Server.TLS.KeyFile == "" {
			appLogger.Error("TLS is enabled but CertFile or KeyFile is missing in configuration.", nil)
			return fmt.Errorf("TLS is enabled but CertFile or KeyFile is missing")
		}

		appLogger.Info("TLS is enabled. Loading certificate and key.", logger.LogFields{
			"cert_file": *cfg.Server.TLS.CertFile,
			"key_file":  *cfg.Server.TLS.KeyFile,
		})

		cert, err := tls.LoadX509KeyPair(*cfg.Server.TLS.CertFile, *cfg.Server.TLS.KeyFile)
		if err != nil {
			appLogger.Error("Failed to load TLS certificate/key", logger.LogFields{"error": err.Error()})
			return fmt.Errorf("failed to load TLS certificate/key: %w", err)
		}
		tlsConfiguration = &tls.Config{
			Certificates: []tls.Certificate{cert},
			NextProtos:   []string{"h2", "http/1.1"}, // ALPN for HTTP/2
			MinVersion:   tls.VersionTLS12,           // Set default minimum TLS version
		}

		// Override default MinVersion if specified in config
		if cfg.Server.TLS.MinVersion != nil && *cfg.Server.TLS.MinVersion != "" {
			switch *cfg.Server.TLS.MinVersion {
			case "1.3":
				tlsConfiguration.MinVersion = tls.VersionTLS13
				// "1.2" is the default, so no case needed for it.
				// Validation in config.go ensures only valid versions are present.
			}
			appLogger.Info("Applied minimum TLS version from configuration.", logger.LogFields{"min_version": *cfg.Server.TLS.MinVersion})
		}
		appLogger.Info("TLS is not enabled or not configured.", nil)
	}

	http2Server, err := server.NewServer(cfg, appLogger, appRouter, originalCfgPath, handlerRegistry, tlsConfiguration)
	if err != nil {
		appLogger.Error("Failed to initialize server", logger.LogFields{"error": err.Error()})
		return fmt.Errorf("failed to initialize server: %w", err)
	}
	appLogger.Info("HTTP/2 server instance created.", nil)

	// Start the server. This is a blocking call.
	appLogger.Info("Starting HTTP/2 server...", logger.LogFields{"address": cfg.Server.Address})
	if err := http2Server.Start(); err != nil {
		appLogger.Error("Server exited with an error", logger.LogFields{"error": err.Error()})
		return fmt.Errorf("server exited with an error: %w", err)
	}

	// If http2Server.Start() returns nil, it means a graceful shutdown completed.
	appLogger.Info("Server has shut down gracefully.", nil)
	return nil
}

func main() {
	// CLI arguments
	flag.StringVar(&configFilePath, "config", "", "Path to the configuration file (JSON or TOML)")
	flag.Parse()

	if configFilePath == "" {
		fmt.Fprintln(os.Stderr, "Error: Configuration file path must be provided via -config flag.")
		flag.Usage()
		os.Exit(1)
	}

	absConfigPath, err := filepath.Abs(configFilePath)
	if err != nil {
		// Use standard log before logger is initialized
		log.Fatalf("Error getting absolute path for config file %s: %v", configFilePath, err)
	}
	configFilePath = absConfigPath // Use absolute path from here

	// 1. Load Configuration
	cfg, err := config.LoadConfig(configFilePath)
	if err != nil {
		// Manually construct and print a JSON log entry to stderr for startup errors,
		// as the full logger might not be initialized.
		errorLogEntry := struct {
			Timestamp string `json:"ts"`
			Level     string `json:"level"`
			Msg       string `json:"msg"`
			Source    string `json:"source,omitempty"`
		}{
			Timestamp: time.Now().UTC().Format("2006-01-02T15:04:05.000Z"),
			Level:     "ERROR",
			Msg:       fmt.Sprintf("Failed to load configuration from %s: %v", configFilePath, err),
			Source:    "cmd/server/main.go:config_load_error",
		}
		jsonErrorBytes, marshalErr := json.Marshal(errorLogEntry)
		if marshalErr != nil {
			fmt.Fprintf(os.Stderr, "CRITICAL: Failed to marshal startup error to JSON: %v. Original error: %v\n", marshalErr, err)
		} else {
			fmt.Fprintln(os.Stderr, string(jsonErrorBytes))
		}
		os.Exit(1)
	}

	// Call the new setup and run function
	if err := setupAndRunServer(cfg, configFilePath); err != nil {
		// setupAndRunServer should have already logged details using the initialized logger.
		// If setupAndRunServer returns an error, it means something went wrong during server setup or execution.
		// The logger inside setupAndRunServer would have logged specifics.
		// We print a general message here to stderr and exit.
		// For critical init errors *before* logger, setupAndRunServer itself might use fmt.Fprintf.
		fmt.Fprintf(os.Stderr, "Server setup or execution failed: %v\n", err)
		os.Exit(1)
	}

	// If setupAndRunServer returns nil, it means a graceful shutdown completed.
	// The logger within setupAndRunServer would have logged "Server has shut down gracefully."
	// The deferred logger close in setupAndRunServer will also execute.
	os.Exit(0)
}
