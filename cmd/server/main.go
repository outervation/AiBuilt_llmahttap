package main

import (
	"flag"
	"fmt"

	"encoding/json"
	"log"
	"os"
	"path/filepath"

	"example.com/llmahttap/v2/internal/config"
	"example.com/llmahttap/v2/internal/logger"
	"example.com/llmahttap/v2/internal/router"
	"example.com/llmahttap/v2/internal/server"

	"example.com/llmahttap/v2/internal/handlers/staticfile" // Assumed to exist for StaticFileServer
)

var (
	configFilePath string
)

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
		log.Fatalf("Error getting absolute path for config file %s: %v", configFilePath, err)
	}
	configFilePath = absConfigPath

	// 1. Load Configuration
	cfg, err := config.LoadConfig(configFilePath)
	if err != nil {
		log.Fatalf("Failed to load configuration from %s: %v", configFilePath, err)
	}

	// 2. Initialize Logger
	appLogger, err := logger.NewLogger(cfg.Logging)
	if err != nil {
		log.Fatalf("Failed to initialize logger: %v", err)
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
	// The factory function is a closure, capturing 'configFilePath' and 'appLogger' from the main scope.
	staticFileServerFactory := func(handlerConfig json.RawMessage, factoryLogger *logger.Logger) (server.Handler, error) {
		// 'configFilePath' is the absolute path to the main config file, available from main() scope.
		// It's used by ParseAndValidateStaticFileServerConfig to resolve relative paths within the static server's config (e.g., MimeTypesPath).
		staticServerSpecificConfig, err := config.ParseAndValidateStaticFileServerConfig(handlerConfig, configFilePath)
		if err != nil {
			// The factory returns an error; the server/router layer (which calls CreateHandler, which calls this factory)
			// will be responsible for logging this appropriately and likely returning a 500 error for the request.
			return nil, fmt.Errorf("StaticFileServer: failed to parse/validate specific handler config: %w", err)
		}

		// Assume staticfile.New constructor exists in example.com/llmahttap/v2/internal/handlers/staticfile
		// and its signature is: func New(cfg *config.StaticFileServerConfig, lg *logger.Logger) (server.Handler, error)
		handler, err := staticfile.New(staticServerSpecificConfig, factoryLogger) // Pass the logger provided to the factory
		if err != nil {
			return nil, fmt.Errorf("StaticFileServer: failed to create handler instance: %w", err)
		}
		return handler, nil
	}

	if err := handlerRegistry.Register("StaticFileServer", staticFileServerFactory); err != nil {
		appLogger.Error("Failed to register StaticFileServer handler factory", logger.LogFields{"error": err.Error()})
		os.Exit(1) // Critical if core handler type registration fails
	}
	appLogger.Info("Registered StaticFileServer handler factory.", nil)

	// Example of how another handler might be registered:
	// myOtherHandlerFactory := func(handlerConfig json.RawMessage, factoryLogger *logger.Logger) (server.Handler, error) {
	//     // ... parse handlerConfig, create and return handler ...
	//     return myotherhandler.New(handlerConfig, factoryLogger)
	// }
	// if err := handlerRegistry.Register("MyOtherHandler", myOtherHandlerFactory); err != nil {
	//     appLogger.Error("Failed to register MyOtherHandler handler factory", logger.LogFields{"error": err.Error()})
	//     os.Exit(1)
	// }
	// appLogger.Info("Registered MyOtherHandler handler factory.", nil)

	// (Actual handler registration will be done in subsequent steps when handlers are implemented)
	if handlerRegistry == nil { // Should not happen if NewHandlerRegistry is correct
		appLogger.Error("Handler registry is nil after initialization", nil)
		os.Exit(1)
	}
	appLogger.Info("Handler registry initialized.", nil)
	// (Actual handler registration will be done in subsequent steps when handlers are implemented)

	// 4. Initialize Router
	// The router needs the routes from the config and the handler registry.
	var routesToUse []config.Route
	if cfg.Routing != nil {
		routesToUse = cfg.Routing.Routes
	}
	appRouter, err := router.NewRouter(routesToUse, handlerRegistry, appLogger)
	if err != nil {
		appLogger.Error("Failed to initialize router", logger.LogFields{"error": err.Error()})
		os.Exit(1)
	}
	appLogger.Info("Router initialized", nil)

	// 5. Initialize Server
	// NewServer(cfg *config.Config, lg *logger.Logger, router RouterInterface, originalCfgPath string, registry *HandlerRegistry)
	http2Server, err := server.NewServer(cfg, appLogger, appRouter, configFilePath, handlerRegistry)
	if err != nil {
		appLogger.Error("Failed to initialize server", logger.LogFields{"error": err.Error()})
		os.Exit(1)
	}
	appLogger.Info("HTTP/2 server instance created.", nil)

	// Start the server. This is a blocking call that will only return when
	// the server shuts down (either gracefully or due to an error).
	// Signal handling (SIGINT, SIGTERM, SIGHUP) is managed internally by the Server instance.
	appLogger.Info("Starting HTTP/2 server...", logger.LogFields{"address": cfg.Server.Address})

	if err := http2Server.Start(); err != nil {
		appLogger.Error("Server exited with an error", logger.LogFields{"error": err.Error()})
		// The deferred appLogger.CloseLogFiles() will run automatically on exit.
		os.Exit(1)
	}

	// If http2Server.Start() returns nil, it means a graceful shutdown completed.
	appLogger.Info("Server has shut down gracefully. Main application exiting.", nil)
	// The deferred appLogger.CloseLogFiles() will run automatically on exit.
	os.Exit(0)
}
