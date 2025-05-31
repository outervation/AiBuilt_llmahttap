package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"example.com/llmahttap/v2/internal/config"
	"example.com/llmahttap/v2/internal/handlers/staticfileserver"
	"example.com/llmahttap/v2/internal/logger"
	"example.com/llmahttap/v2/internal/router"
	"example.com/llmahttap/v2/internal/server"
)

func main() {
	_ = fmt.Sprintf("") // Dummy use of fmt to satisfy import requirement and compiler
	// Parse command-line arguments
	rootDir := flag.String("root", ".", "Root directory to serve files from")
	port := flag.String("port", "8080", "Port to listen on")
	flag.Parse()

	// Resolve absolute path for DocumentRoot
	absRootDir, err := filepath.Abs(*rootDir)
	if err != nil {
		log.Fatalf("Error resolving absolute path for root directory: %v", err)
	}

	// Programmatically construct config.Config
	serverAddr := "0.0.0.0:" + *port
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
		log.Fatalf("Error marshalling StaticFileServerConfig: %v", err)
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

	// Initialize logger
	lg, err := logger.NewLogger(cfg.Logging)
	if err != nil {
		log.Fatalf("Error initializing logger: %v", err)
	}

	// Create HandlerRegistry
	handlerRegistry := server.NewHandlerRegistry()

	// Register StaticFileServer handler factory
	err = handlerRegistry.Register("StaticFileServer", func(hc json.RawMessage, l *logger.Logger) (server.Handler, error) {
		// The mainConfigFilePath is empty because MimeTypesPath is not being used here.
		// If MimeTypesPath were used, we'd need to pass a valid path or handle it.
		return staticfileserver.New(hc, l, "")
	})
	if err != nil {
		lg.Error("Error registering StaticFileServer handler", logger.LogFields{"error": err.Error()})
		os.Exit(1)
	}

	// Create Router
	rt, err := router.NewRouter(cfg.Routing.Routes, handlerRegistry, lg)
	if err != nil {
		lg.Error("Error creating router", logger.LogFields{"error": err.Error()})
		os.Exit(1)
	}

	// Create Server
	// originalCfgPath is empty as config is programmatic
	srv, err := server.NewServer(cfg, lg, rt, "", handlerRegistry)
	if err != nil {
		lg.Error("Error creating server", logger.LogFields{"error": err.Error()})
		os.Exit(1)
	}

	// Start the server
	lg.Info("Starting HTTP/2 server...", logger.LogFields{"address": serverAddr, "root": absRootDir})
	if err := srv.Start(); err != nil {
		lg.Error("Server failed to start", logger.LogFields{"error": err.Error()})
		os.Exit(1)
	}

	lg.Info("Server shut down gracefully.", nil)
}
