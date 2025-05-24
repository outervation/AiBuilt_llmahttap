package config

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net"
	"path/filepath"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// Duration is a wrapper around time.Duration to allow for custom
// unmarshalling from string values in configuration files.
type Duration time.Duration

// UnmarshalText implements the encoding.TextUnmarshaler interface.
// This is used by TOML and potentially other text-based formats.
func (d *Duration) UnmarshalText(text []byte) error {
	if len(text) == 0 {
		// Handle empty string case, perhaps by setting a default or returning an error
		// For now, let's assume empty string means "not set" or "use default"
		// which should be handled by the calling code or applyDefaults.
		// Or, treat as an error:
		return fmt.Errorf("duration string cannot be empty")
	}
	dur, err := time.ParseDuration(string(text))
	if err != nil {
		return fmt.Errorf("invalid duration string %q: %w", string(text), err)
	}
	if dur <= 0 {
		return fmt.Errorf("duration must be positive, got %q", string(text))
	}
	*d = Duration(dur)
	return nil
}

// UnmarshalJSON implements the json.Unmarshaler interface.
func (d *Duration) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return fmt.Errorf("duration should be a string, got %s: %w", string(data), err)
	}
	// Reuse UnmarshalText logic after converting string from JSON quotes
	return d.UnmarshalText([]byte(s))
}

// String returns the string representation of the Duration.
func (d Duration) String() string {
	return time.Duration(d).String()
}

// Value returns the underlying time.Duration.
func (d Duration) Value() time.Duration {
	return time.Duration(d)
}

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
	Server           *ServerConfig  `json:"server,omitempty" toml:"server,omitempty"`
	Routing          *RoutingConfig `json:"routing,omitempty" toml:"routing,omitempty"`
	Logging          *LoggingConfig `json:"logging,omitempty" toml:"logging,omitempty"`
	originalFilePath string         // Internal: path to the loaded config file
}

// ServerConfig holds general server settings.
type ServerConfig struct {
	Address                 *string `json:"address,omitempty" toml:"address,omitempty"` // e.g., ":443", "localhost:8080"
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
	DocumentRoot          string            `json:"document_root" toml:"document_root"`
	IndexFiles            []string          `json:"index_files,omitempty" toml:"index_files,omitempty"`
	ServeDirectoryListing *bool             `json:"serve_directory_listing,omitempty" toml:"serve_directory_listing,omitempty"`
	MimeTypesPath         *string           `json:"mime_types_path,omitempty" toml:"mime_types_path,omitempty"` // Path to JSON file for MIME types
	MimeTypesMap          map[string]string `json:"mime_types_map,omitempty" toml:"mime_types_map,omitempty"`   // Inline MIME type map
	ResolvedMimeTypes     map[string]string `json:"-" toml:"-"`                                                 // Not from config, resolved later by handler
}

// TODO: Implement loading, parsing (JSON, TOML with auto-detect), and validation functions.
// TODO: Implement functions to apply default values to the configuration.

const (
	defaultServerAddress           = ":443"
	defaultChildReadinessTimeout   = "10s"
	defaultGracefulShutdownTimeout = "30s"
	defaultLogLevel                = LogLevelInfo
	defaultAccessLogEnabled        = true
	defaultAccessLogTarget         = "stdout"
	defaultAccessLogFormat         = "json"
	defaultAccessLogRealIPHeader   = "X-Forwarded-For"
	defaultErrorLogTarget          = "stderr"
)

// LoadConfig reads the configuration file from the given path,
// auto-detects its format (JSON or TOML), parses it, applies defaults,
// and validates it.
func LoadConfig(filePath string) (*Config, error) {
	if filePath == "" {
		return nil, fmt.Errorf("configuration file path cannot be empty")
	}

	data, err := ioutil.ReadFile(filePath)
	if err != nil {
		return nil, fmt.Errorf("failed to read configuration file %s: %w", filePath, err)
	}

	cfg := &Config{}
	fileExt := strings.ToLower(filepath.Ext(filePath))

	parsed := false
	var parseErr error

	if fileExt == ".json" {
		err = json.Unmarshal(data, cfg)
		if err == nil {
			parsed = true
		} else {
			parseErr = fmt.Errorf("failed to parse JSON config %s: %w", filePath, err)
		}
	} else if fileExt == ".toml" {
		err = toml.Unmarshal(data, cfg)
		if err == nil {
			parsed = true
		} else {
			parseErr = fmt.Errorf("failed to parse TOML config %s: %w", filePath, err)
		}
	} else {
		// Auto-detect content if extension is not .json or .toml
		// Try JSON first
		errJSON := json.Unmarshal(data, cfg) // cfg is the main Config struct instance
		if errJSON == nil {
			parsed = true
		} else {
			// JSON parsing failed, try TOML.
			// Note: 'cfg' might be partially modified by the failed JSON unmarshal.
			// We create a new Config instance for TOML and assign it to 'cfg' only on success.
			cfgTOMLAttempt := &Config{}
			errTOML := toml.Unmarshal(data, cfgTOMLAttempt)
			if errTOML == nil {
				cfg = cfgTOMLAttempt // Replace original 'cfg' with the successfully parsed TOML one
				parsed = true
			} else {
				parseErr = fmt.Errorf("failed to auto-detect and parse config %s: JSON error: %v, TOML error: %v", filePath, errJSON, errTOML)
			}
		}
	}

	if !parsed {
		return nil, parseErr
	}

	applyDefaults(cfg)

	if err := validateConfig(cfg); err != nil {
		return nil, fmt.Errorf("configuration validation failed: %w", err)
	}

	cfg.originalFilePath = filePath // Store the path
	return cfg, nil
}

// OriginalFilePath returns the path from which the configuration was loaded.
// This is used for reloading the configuration on SIGHUP.
func (c *Config) OriginalFilePath() string {
	if c == nil {
		return ""
	}
	return c.originalFilePath
}
func applyDefaults(cfg *Config) {
	if cfg.Server == nil {
		cfg.Server = &ServerConfig{}
	}
	if cfg.Server.ChildReadinessTimeout == nil {
		val := defaultChildReadinessTimeout
		cfg.Server.ChildReadinessTimeout = &val
	}
	if cfg.Server.GracefulShutdownTimeout == nil {
		val := defaultGracefulShutdownTimeout
		cfg.Server.GracefulShutdownTimeout = &val
	}

	if cfg.Logging == nil {
		cfg.Logging = &LoggingConfig{}
	}
	if cfg.Logging.LogLevel == "" {
		cfg.Logging.LogLevel = defaultLogLevel
	}

	if cfg.Logging.AccessLog == nil {
		cfg.Logging.AccessLog = &AccessLogConfig{}
	}
	if cfg.Logging.AccessLog.Enabled == nil {
		val := defaultAccessLogEnabled
		cfg.Logging.AccessLog.Enabled = &val
	}
	if cfg.Logging.AccessLog.Target == "" {
		cfg.Logging.AccessLog.Target = defaultAccessLogTarget
	}
	if cfg.Logging.AccessLog.Format == "" {
		cfg.Logging.AccessLog.Format = defaultAccessLogFormat
	}
	if cfg.Logging.AccessLog.RealIPHeader == nil {
		val := defaultAccessLogRealIPHeader
		cfg.Logging.AccessLog.RealIPHeader = &val
	}
	// Ensure TrustedProxies is not nil if not provided, for easier use later
	if cfg.Logging.AccessLog.TrustedProxies == nil {
		cfg.Logging.AccessLog.TrustedProxies = []string{}
	}

	if cfg.Logging.ErrorLog == nil {
		cfg.Logging.ErrorLog = &ErrorLogConfig{}
	}
	if cfg.Logging.ErrorLog.Target == "" {
		cfg.Logging.ErrorLog.Target = defaultErrorLogTarget
	}

	// Defaults for StaticFileServerConfig
	if cfg.Routing != nil {
		for i := range cfg.Routing.Routes {
			if cfg.Routing.Routes[i].HandlerType == "StaticFileServer" {
				// This is tricky because HandlerConfig is json.RawMessage.
				// Actual unmarshalling and default application for specific handlers
				// should ideally happen when the handler is instantiated.
				// However, we can set defaults for commonly expected StaticFileServer fields
				// if we unmarshal it temporarily here, or document that handlers must manage their own defaults.
				// For now, let's assume handlers manage their own full default setup.
				// Example for IndexFiles if we were to do it here:
				/*
				   var sfsCfg StaticFileServerConfig
				   if cfg.Routing.Routes[i].HandlerConfig != nil {
				       if err := json.Unmarshal(cfg.Routing.Routes[i].HandlerConfig, &sfsCfg); err == nil {
				           if sfsCfg.IndexFiles == nil {
				               sfsCfg.IndexFiles = []string{"index.html"}
				           }
				           if sfsCfg.ServeDirectoryListing == nil {
				               b := false
				               sfsCfg.ServeDirectoryListing = &b
				           }
				           // Re-marshal back to HandlerConfig if modified
				           // updatedHandlerConfig, _ := json.Marshal(sfsCfg)
				           // cfg.Routing.Routes[i].HandlerConfig = updatedHandlerConfig
				       }
				   } else {
				       // If HandlerConfig is nil, create a default one
				       // sfsCfg.IndexFiles = []string{"index.html"}
				       // b := false
				       // sfsCfg.ServeDirectoryListing = &b
				       // defaultHandlerConfig, _ := json.Marshal(sfsCfg)
				       // cfg.Routing.Routes[i].HandlerConfig = defaultHandlerConfig
				   }
				*/
			}
		}
	}
}

func parseDuration(s *string, fieldName string, defaultValue string) (time.Duration, error) {
	valStr := defaultValue
	if s != nil && *s != "" { // only use s if it's non-nil and non-empty
		valStr = *s
	} else if s != nil && *s == "" { // if explicitly set to empty string, it's an error for durations
		return 0, fmt.Errorf("%s cannot be an empty string if specified; omit or provide valid duration", fieldName)
	}

	if valStr == "" { // Should only happen if defaultValue is empty, which is not the case for our durations
		return 0, fmt.Errorf("%s has no value or default value for parsing", fieldName)
	}

	d, err := time.ParseDuration(valStr)
	if err != nil {
		return 0, fmt.Errorf("invalid format for %s '%s': %w", fieldName, valStr, err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("%s must be a positive duration, got '%s'", fieldName, valStr)
	}
	return d, nil
}

func validateConfig(cfg *Config) error {
	if cfg.Server == nil {
		// Default is applied, so this shouldn't happen unless applyDefaults is bypassed.
		return fmt.Errorf("server configuration section is effectively missing after defaults")
	}
	if cfg.Server.ExecutablePath != nil && *cfg.Server.ExecutablePath == "" {
		return fmt.Errorf("server.executable_path, if provided, cannot be empty")
	}
	// Validate durations using the parseDuration helper which also applies defaults for validation
	if _, err := parseDuration(cfg.Server.ChildReadinessTimeout, "server.child_readiness_timeout", defaultChildReadinessTimeout); err != nil {
		return err
	}
	if _, err := parseDuration(cfg.Server.GracefulShutdownTimeout, "server.graceful_shutdown_timeout", defaultGracefulShutdownTimeout); err != nil {
		return err
	}

	if cfg.Routing == nil {
		// Default is applied, making it &RoutingConfig{Routes: []Route{}}
		// This state is considered valid (no routes).
	}

	routeSignatures := make(map[string]struct{})
	if cfg.Routing != nil {
		for i, route := range cfg.Routing.Routes {
			if route.PathPattern == "" {
				return fmt.Errorf("routing.routes[%d].path_pattern cannot be empty", i)
			}
			if route.HandlerType == "" {
				return fmt.Errorf("routing.routes[%d].handler_type cannot be empty for path_pattern '%s'", i, route.PathPattern)
			}

			switch route.MatchType {
			case MatchTypeExact:
				if strings.HasSuffix(route.PathPattern, "/") && route.PathPattern != "/" {
					return fmt.Errorf("routing.routes[%d].path_pattern '%s' with MatchType 'Exact' must not end with '/' unless it is the root path '/'", i, route.PathPattern)
				}
			case MatchTypePrefix:
				if !strings.HasSuffix(route.PathPattern, "/") {
					return fmt.Errorf("routing.routes[%d].path_pattern '%s' with MatchType 'Prefix' must end with '/'", i, route.PathPattern)
				}
			case "": // MatchType not specified
				return fmt.Errorf("routing.routes[%d].match_type is missing for path_pattern '%s'; must be 'Exact' or 'Prefix'", i, route.PathPattern)
			default:
				return fmt.Errorf("routing.routes[%d].match_type '%s' is invalid for path_pattern '%s'; must be 'Exact' or 'Prefix'", i, route.MatchType, route.PathPattern)
			}

			signature := route.PathPattern + string(route.MatchType)
			if _, exists := routeSignatures[signature]; exists {
				return fmt.Errorf("ambiguous route: duplicate PathPattern '%s' and MatchType '%s' found at routing.routes[%d]", route.PathPattern, route.MatchType, i)
			}
			routeSignatures[signature] = struct{}{}

			// Validate StaticFileServerConfig if HandlerType matches
			if route.HandlerType == "StaticFileServer" {
				if route.HandlerConfig == nil || len(route.HandlerConfig) == 0 {
					return fmt.Errorf("routing.routes[%d].handler_config is missing for HandlerType 'StaticFileServer' with path_pattern '%s'", i, route.PathPattern)
				}
				var sfsCfg StaticFileServerConfig
				if err := json.Unmarshal(route.HandlerConfig, &sfsCfg); err != nil {
					return fmt.Errorf("routing.routes[%d].handler_config for HandlerType 'StaticFileServer' (path_pattern '%s') is invalid: %w", i, route.PathPattern, err)
				}
				if sfsCfg.DocumentRoot == "" {
					return fmt.Errorf("routing.routes[%d].handler_config.document_root is required for HandlerType 'StaticFileServer' (path_pattern '%s')", i, route.PathPattern)
				}
				if !filepath.IsAbs(sfsCfg.DocumentRoot) {
					return fmt.Errorf("routing.routes[%d].handler_config.document_root '%s' must be an absolute path for HandlerType 'StaticFileServer' (path_pattern '%s')", i, sfsCfg.DocumentRoot, route.PathPattern)
				}

				// Validate MimeTypesPath and MimeTypesMap
				if sfsCfg.MimeTypesPath != nil && len(sfsCfg.MimeTypesMap) > 0 {
					return fmt.Errorf("routing.routes[%d].handler_config: MimeTypesPath ('%s') and MimeTypesMap cannot both be specified for path_pattern '%s'. Provide one or the other.", i, *sfsCfg.MimeTypesPath, route.PathPattern)
				}

				if sfsCfg.MimeTypesPath != nil {
					if *sfsCfg.MimeTypesPath == "" {
						return fmt.Errorf("routing.routes[%d].handler_config.mime_types_path cannot be empty if specified (path_pattern '%s')", i, route.PathPattern)
					}
					// Further validation (e.g., file existence, JSON format) can be done by the handler at initialization.
				}

				if len(sfsCfg.MimeTypesMap) > 0 {
					for k, v := range sfsCfg.MimeTypesMap {
						if !strings.HasPrefix(k, ".") {
							return fmt.Errorf("routing.routes[%d].handler_config.mime_types_map key '%s' must start with a '.' (path_pattern '%s')", i, k, route.PathPattern)
						}
						if v == "" { // MIME type value should not be empty
							return fmt.Errorf("routing.routes[%d].handler_config.mime_types_map value for key '%s' cannot be empty (path_pattern '%s')", i, k, route.PathPattern)
						}
					}
				}
			}
		}
	}

	if cfg.Logging == nil {
		// Default is applied.
		return fmt.Errorf("logging configuration section is effectively missing after defaults")
	}
	switch cfg.Logging.LogLevel {
	case LogLevelDebug, LogLevelInfo, LogLevelWarning, LogLevelError:
		// valid
	default:
		return fmt.Errorf("logging.log_level '%s' is invalid; must be one of 'DEBUG', 'INFO', 'WARNING', 'ERROR'", cfg.Logging.LogLevel)
	}

	if cfg.Logging.AccessLog == nil {
		// Default is applied.
		return fmt.Errorf("logging.access_log section is effectively missing after defaults")
	}
	if cfg.Logging.AccessLog.Target == "" { // Should be caught by default
		return fmt.Errorf("logging.access_log.target cannot be empty")
	}
	if isFilePath(cfg.Logging.AccessLog.Target) && !filepath.IsAbs(cfg.Logging.AccessLog.Target) {
		return fmt.Errorf("logging.access_log.target path '%s' must be absolute", cfg.Logging.AccessLog.Target)
	}
	if cfg.Logging.AccessLog.Format != "json" { // Currently only "json" is supported
		return fmt.Errorf("logging.access_log.format '%s' is invalid; currently only 'json' is supported", cfg.Logging.AccessLog.Format)
	}
	if cfg.Logging.AccessLog.RealIPHeader != nil && *cfg.Logging.AccessLog.RealIPHeader == "" {
		return fmt.Errorf("logging.access_log.real_ip_header, if provided, cannot be empty")
	}
	for _, cidr := range cfg.Logging.AccessLog.TrustedProxies {
		if _, _, err := net.ParseCIDR(cidr); err != nil {
			if net.ParseIP(cidr) == nil {
				return fmt.Errorf("logging.access_log.trusted_proxies entry '%s' is not a valid CIDR or IP address: %w", cidr, err)
			}
		}
	}

	if cfg.Logging.ErrorLog == nil {
		// Default is applied.
		return fmt.Errorf("logging.error_log section is effectively missing after defaults")
	}
	if cfg.Logging.ErrorLog.Target == "" { // Should be caught by default
		return fmt.Errorf("logging.error_log.target cannot be empty")
	}
	if isFilePath(cfg.Logging.ErrorLog.Target) && !filepath.IsAbs(cfg.Logging.ErrorLog.Target) {
		return fmt.Errorf("logging.error_log.target path '%s' must be absolute", cfg.Logging.ErrorLog.Target)
	}

	return nil
}

// isFilePath checks if a target string is likely a file path (not stdout/stderr).
func isFilePath(target string) bool {
	return target != "stdout" && target != "stderr"
}

// Helper function to unmarshal StaticFileServerConfig specifically for validation or default application
// This isn't strictly necessary if handlers do all their own unmarshalling and validation
// but can be useful for centralized validation logic.
func unmarshalStaticFileServerConfig(rawConfig json.RawMessage) (*StaticFileServerConfig, error) {
	if rawConfig == nil {
		return nil, fmt.Errorf("handler config is nil")
	}
	var sfsCfg StaticFileServerConfig
	if err := json.Unmarshal(rawConfig, &sfsCfg); err != nil {
		return nil, fmt.Errorf("failed to unmarshal StaticFileServerConfig: %w", err)
	}

	// Apply defaults specific to StaticFileServerConfig
	if sfsCfg.IndexFiles == nil {
		sfsCfg.IndexFiles = []string{"index.html"}
	}
	if sfsCfg.ServeDirectoryListing == nil {
		b := false
		sfsCfg.ServeDirectoryListing = &b
	}

	return &sfsCfg, nil
}
