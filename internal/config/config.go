package config

import (
	"encoding/json"
)

// MatchType defines how a path pattern is interpreted.
type MatchType string

const (
	// MatchTypeExact matches the path exactly.
	MatchTypeExact MatchType = "Exact"
	// MatchTypePrefix matches any path starting with the prefix.
	MatchTypePrefix MatchType = "Prefix"
)

// LogLevel defines the minimum severity for error logs.
type LogLevel string

const (
	LogLevelDebug   LogLevel = "DEBUG"
	LogLevelInfo    LogLevel = "INFO"
	LogLevelWarning LogLevel = "WARNING"
	LogLevelError   LogLevel = "ERROR"
)

// Config is the top-level configuration structure for the server.
type Config struct {
	Server  *ServerConfig  `json:"server,omitempty" toml:"server,omitempty"`
	Routing *RoutingConfig `json:"routing,omitempty" toml:"routing,omitempty"`
	Logging *LoggingConfig `json:"logging,omitempty" toml:"logging,omitempty"`
}

// ServerConfig holds general server settings.
type ServerConfig struct {
	ExecutablePath          *string `json:"executable_path,omitempty" toml:"executable_path,omitempty"`
	ChildReadinessTimeout   *string `json:"child_readiness_timeout,omitempty" toml:"child_readiness_timeout,omitempty"`     // e.g., "10s"
	GracefulShutdownTimeout *string `json:"graceful_shutdown_timeout,omitempty" toml:"graceful_shutdown_timeout,omitempty"` // e.g., "30s"
}

// RoutingConfig contains the list of routes.
type RoutingConfig struct {
	Routes []Route `json:"routes,omitempty" toml:"routes,omitempty"`
}

// Route defines a single routing rule.
type Route struct {
	PathPattern   string          `json:"path_pattern" toml:"path_pattern"`
	MatchType     MatchType       `json:"match_type" toml:"match_type"`
	HandlerType   string          `json:"handler_type" toml:"handler_type"`
	HandlerConfig json.RawMessage `json:"handler_config,omitempty" toml:"handler_config,omitempty"`
}

// LoggingConfig holds logging configurations.
type LoggingConfig struct {
	LogLevel  LogLevel         `json:"log_level,omitempty" toml:"log_level,omitempty"`
	AccessLog *AccessLogConfig `json:"access_log,omitempty" toml:"access_log,omitempty"`
	ErrorLog  *ErrorLogConfig  `json:"error_log,omitempty" toml:"error_log,omitempty"`
}

// AccessLogConfig configures access logging.
type AccessLogConfig struct {
	Enabled        *bool    `json:"enabled,omitempty" toml:"enabled,omitempty"`
	Target         string   `json:"target,omitempty" toml:"target,omitempty"`
	Format         string   `json:"format,omitempty" toml:"format,omitempty"`
	TrustedProxies []string `json:"trusted_proxies,omitempty" toml:"trusted_proxies,omitempty"`
	RealIPHeader   *string  `json:"real_ip_header,omitempty" toml:"real_ip_header,omitempty"`
}

// ErrorLogConfig configures error logging.
type ErrorLogConfig struct {
	Target string `json:"target,omitempty" toml:"target,omitempty"`
}

// StaticFileServerConfig is the specific HandlerConfig for "StaticFileServer" type routes.
// This is an example of how a specific handler config would be defined.
// It will be unmarshalled from Route.HandlerConfig (json.RawMessage).
type StaticFileServerConfig struct {
	DocumentRoot          string      `json:"document_root" toml:"document_root"`
	IndexFiles            []string    `json:"index_files,omitempty" toml:"index_files,omitempty"`
	ServeDirectoryListing *bool       `json:"serve_directory_listing,omitempty" toml:"serve_directory_listing,omitempty"`
	MimeTypes             interface{} `json:"mime_types,omitempty" toml:"mime_types,omitempty"` // Can be map[string]string or string (path to JSON file)
}

// TODO: Implement loading, parsing (JSON, TOML with auto-detect), and validation functions.
// TODO: Implement functions to apply default values to the configuration.
