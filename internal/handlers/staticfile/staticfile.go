package staticfile

import (
	"fmt"
	"net/http"

	"example.com/llmahttap/v2/internal/config"
	"example.com/llmahttap/v2/internal/http2" // For http2.StreamWriter
	"example.com/llmahttap/v2/internal/logger"
	"example.com/llmahttap/v2/internal/server" // For server.Handler
)

// StaticFileServer handles serving static files.
type StaticFileServer struct {
	config *config.StaticFileServerConfig
	logger *logger.Logger
}

// New creates a new StaticFileServer handler.
// It conforms to the server.HandlerFactory signature when partially applied or wrapped.
func New(cfg *config.StaticFileServerConfig, lg *logger.Logger) (server.Handler, error) {
	if cfg == nil {
		return nil, fmt.Errorf("staticfileserver: config cannot be nil")
	}
	if lg == nil {
		return nil, fmt.Errorf("staticfileserver: logger cannot be nil")
	}
	// Further validation of cfg specific to static file server could happen here
	// e.g., checking if DocumentRoot is valid, etc.
	// For now, config.ParseAndValidateStaticFileServerConfig handles most of it.

	return &StaticFileServer{
		config: cfg,
		logger: lg,
	}, nil
}

// ServeHTTP2 implements the server.Handler interface.
// This is a placeholder implementation.
func (s *StaticFileServer) ServeHTTP2(stream http2.StreamWriter, req *http.Request) {
	s.logger.Info("StaticFileServer: ServeHTTP2 called (placeholder)", logger.LogFields{
		"path":   req.URL.Path,
		"method": req.Method,
	})

	// Placeholder response
	headers := []http2.HeaderField{
		{Name: ":status", Value: "200"},
		{Name: "content-type", Value: "text/plain; charset=utf-8"},
	}
	body := []byte("Hello from StaticFileServer (placeholder)")
	headers = append(headers, http2.HeaderField{Name: "content-length", Value: fmt.Sprintf("%d", len(body))})

	if err := stream.SendHeaders(headers, false); err != nil {
		s.logger.Error("StaticFileServer: Failed to send headers", logger.LogFields{"error": err.Error()})
		// Consider sending RST_STREAM if appropriate and possible
		return
	}
	if _, err := stream.WriteData(body, true); err != nil {
		s.logger.Error("StaticFileServer: Failed to write data", logger.LogFields{"error": err.Error()})
	}
}
