package logger

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/textproto" // Added for CanonicalMIMEHeaderKey
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"example.com/llmahttap/v2/internal/config"
)

// parsedProxiesContainer holds pre-parsed trusted proxy IP addresses and CIDR blocks.
type parsedProxiesContainer struct {
	cidrs []*net.IPNet
	ips   []net.IP
}

// AccessLogEntry defines the structure for a JSON access log entry.
// Fields are based on spec 3.3.2.
type AccessLogEntry struct {
	Timestamp     string `json:"ts"`
	RemoteAddr    string `json:"remote_addr"`
	RemotePort    int    `json:"remote_port"` // Parsed from client's direct peer port string. Spec: Number.
	Protocol      string `json:"protocol"`
	Method        string `json:"method"`
	URI           string `json:"uri"`
	Status        int    `json:"status"`
	ResponseBytes int64  `json:"resp_bytes"`
	DurationMs    int64  `json:"duration_ms"`
	UserAgent     string `json:"user_agent,omitempty"`
	Referer       string `json:"referer,omitempty"`
	H2StreamID    uint32 `json:"h2_stream_id"`
}

// LogFields represents a map of additional key-value data for structured logging.
// This is used for the 'details' field in ErrorLogEntry or for passing custom fields
// to logger methods.
type LogFields map[string]interface{}

// ErrorLogEntry defines the structure for a JSON error log entry.
// Fields are based on spec 3.4.2.
type ErrorLogEntry struct {
	Timestamp         string    `json:"ts"`
	Level             string    `json:"level"` // e.g., "ERROR", "INFO"
	Message           string    `json:"msg"`
	Source            string    `json:"source,omitempty"`       // e.g., "filename.go:123"
	RequestMethod     string    `json:"method,omitempty"`       // Associated request HTTP method
	RequestURI        string    `json:"uri,omitempty"`          // Associated request URI
	RequestH2StreamID uint32    `json:"h2_stream_id,omitempty"` // Associated request HTTP/2 stream ID
	Details           LogFields `json:"details,omitempty"`      // Additional structured key-value context
}

// AccessLogger handles access logging.
type AccessLogger struct {
	logger        *log.Logger
	config        config.AccessLogConfig
	mu            sync.Mutex
	output        io.WriteCloser
	parsedProxies parsedProxiesContainer
}

// ErrorLogger handles error logging.
type ErrorLogger struct {
	logger         *log.Logger
	config         config.ErrorLogConfig
	globalLogLevel config.LogLevel
	mu             sync.Mutex
	output         io.WriteCloser
}

// getTimestamp generates a timestamp string in ISO 8601 UTC format
// with millisecond precision (e.g., "2023-03-15T12:00:00.123Z").
func getTimestamp() string {
	return time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
}

// Logger is a general logger that contains specific loggers for access and errors.
type Logger struct {
	accessLog      *AccessLogger
	errorLog       *ErrorLogger
	globalLogLevel config.LogLevel
}

// NewLogger creates and configures a new Logger instance.
func NewLogger(cfg *config.LoggingConfig) (*Logger, error) {
	if cfg == nil {
		return nil, fmt.Errorf("logging configuration cannot be nil")
	}

	var err error
	l := &Logger{
		globalLogLevel: cfg.LogLevel,
	}

	// Setup Error Logger
	if cfg.ErrorLog != nil {

		var errorOutput io.WriteCloser = os.Stderr // Default
		// Check if ErrorLog.Target is non-nil before dereferencing
		if cfg.ErrorLog.Target != nil && *cfg.ErrorLog.Target != "stderr" {
			if *cfg.ErrorLog.Target == "stdout" {
				errorOutput = os.Stdout
			} else if config.IsFilePath(*cfg.ErrorLog.Target) {
				// Ensure path is absolute (validated in config)
				// TODO: Add file opening logic, SIGHUP handling will need this path
				file, errOpen := os.OpenFile(*cfg.ErrorLog.Target, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
				if errOpen != nil {
					return nil, fmt.Errorf("failed to open error log file %s: %w", *cfg.ErrorLog.Target, errOpen)
				}
				errorOutput = file
			} else {
				// Should not happen if config validation is correct
				return nil, fmt.Errorf("invalid error log target: %s", *cfg.ErrorLog.Target)
			}
		}
		l.errorLog = &ErrorLogger{
			logger:         log.New(errorOutput, "", 0), // No prefix/flags, we format manually
			config:         *cfg.ErrorLog,
			globalLogLevel: cfg.LogLevel,
			output:         errorOutput,
		}
	} else {
		// This case should ideally be prevented by config defaulting.
		// If ErrorLog is nil, we might default to a stderr logger with default LogLevel.
		// For now, let's assume config ensures ErrorLog is non-nil.
		defaultStdErrTarget := "stderr"
		l.errorLog = &ErrorLogger{ // Default to stderr if not configured
			logger:         log.New(os.Stderr, "", 0),
			config:         config.ErrorLogConfig{Target: &defaultStdErrTarget}, // Minimal default
			globalLogLevel: config.LogLevelInfo,                                 // Default log level
			output:         os.Stderr,
		}
	}

	// Setup Access Logger
	if cfg.AccessLog != nil && (cfg.AccessLog.Enabled == nil || *cfg.AccessLog.Enabled) {

		var accessOutput io.WriteCloser = os.Stdout // Default
		// Check if AccessLog.Target is non-nil before dereferencing
		if cfg.AccessLog.Target != nil && *cfg.AccessLog.Target != "stdout" {
			if *cfg.AccessLog.Target == "stderr" {
				accessOutput = os.Stderr
			} else if config.IsFilePath(*cfg.AccessLog.Target) {
				// Ensure path is absolute (validated in config)
				// TODO: Add file opening logic, SIGHUP handling will need this path
				file, errOpen := os.OpenFile(*cfg.AccessLog.Target, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
				if errOpen != nil {
					return nil, fmt.Errorf("failed to open access log file %s: %w", *cfg.AccessLog.Target, errOpen)
				}
				accessOutput = file
			} else {
				// Should not happen if config validation is correct
				return nil, fmt.Errorf("invalid access log target: %s", *cfg.AccessLog.Target)
			}
		}

		parsedProxies, errP := preParseTrustedProxies(cfg.AccessLog.TrustedProxies)
		if errP != nil {
			// Close any opened files before returning error
			if l.errorLog != nil && l.errorLog.output != os.Stderr && l.errorLog.output != os.Stdout {
				l.errorLog.output.Close()
			}
			if accessOutput != os.Stdout && accessOutput != os.Stderr {
				// This specific accessOutput might not be assigned to l.accessLog.output yet
				// but if it was opened, it should be closed.
				if f, ok := accessOutput.(*os.File); ok {
					f.Close()
				}
			}
			return nil, fmt.Errorf("failed to parse trusted proxies for access log: %w", errP)
		}
		l.accessLog = &AccessLogger{
			logger:        log.New(accessOutput, "", 0), // No prefix/flags
			config:        *cfg.AccessLog,
			output:        accessOutput,
			parsedProxies: parsedProxies,
		}
	}

	// TODO: Implement SIGHUP signal handling for log file reopening.

	return l, err
}

// preParseTrustedProxies converts string representations of IPs and CIDRs
// into net.IP and *net.IPNet objects for efficient checking.
func preParseTrustedProxies(proxyStrings []string) (parsedProxiesContainer, error) {
	container := parsedProxiesContainer{
		cidrs: make([]*net.IPNet, 0),
		ips:   make([]net.IP, 0),
	}

	if proxyStrings == nil {
		return container, nil // No proxies to parse
	}

	for _, pStr := range proxyStrings {
		pStr = strings.TrimSpace(pStr)
		if pStr == "" {
			continue
		}
		if strings.Contains(pStr, "/") { // Likely a CIDR
			_, ipNet, err := net.ParseCIDR(pStr)
			if err != nil {
				return parsedProxiesContainer{}, fmt.Errorf("invalid CIDR string in trusted_proxies '%s': %w", pStr, err)
			}
			container.cidrs = append(container.cidrs, ipNet)
		} else { // Likely a single IP
			ip := net.ParseIP(pStr)
			if ip == nil {
				return parsedProxiesContainer{}, fmt.Errorf("invalid IP string in trusted_proxies '%s'", pStr)
			}
			container.ips = append(container.ips, ip)
		}
	}
	return container, nil
}

// isIPTrusted checks if a given IP address is in the list of trusted proxies.
func isIPTrusted(ip net.IP, trustedProxies parsedProxiesContainer) bool {
	if ip == nil {
		return false // A nil IP cannot be trusted
	}
	for _, trustedCIDR := range trustedProxies.cidrs {
		if trustedCIDR.Contains(ip) {
			return true
		}
	}
	for _, trustedIP := range trustedProxies.ips {
		if trustedIP.Equal(ip) {
			return true
		}
	}
	return false
}

// getRealClientIP determines the client's real IP address based on request headers
// and trusted proxy configuration.
// remoteAddr is the direct peer's address (e.g., from http.Request.RemoteAddr).
// headers are the HTTP request headers.
// realIPHeaderName is the name of the header to check (e.g., "X-Forwarded-For").
// trustedProxies contains the pre-parsed list of trusted proxy IPs and CIDRs.

func getRealClientIP(remoteAddr string, headers http.Header, realIPHeaderName string, trustedProxies parsedProxiesContainer) string {
	log.Printf("[getRealClientIP TRACE V4] Entry: remoteAddr=%s, realIPHeaderName='%s' (len: %d)", remoteAddr, realIPHeaderName, len(realIPHeaderName))
	log.Printf("[getRealClientIP TRACE V4] realIPHeaderName (bytes): %v", []byte(realIPHeaderName))

	log.Printf("[getRealClientIP TRACE V4] All headers received in getRealClientIP (map dump):")
	for k, v := range headers {
		log.Printf("[getRealClientIP TRACE V4]   HEADER KEY: '%s' (len: %d, bytes: %v), VALUE: '%v', Get('%s'): '%s'",
			k, len(k), []byte(k), v, k, headers.Get(k))
	}
	if val, ok := headers["CF-Connecting-IP"]; ok { // Test with exact string literal
		log.Printf("[getRealClientIP TRACE V4] Direct map access headers[\"CF-Connecting-IP\"] exists. Value: %v. len: %d", val, len(val))
	} else {
		log.Printf("[getRealClientIP TRACE V4] Direct map access headers[\"CF-Connecting-IP\"] DOES NOT exist.")
	}
	if val, ok := headers[textproto.CanonicalMIMEHeaderKey("CF-Connecting-IP")]; ok { // Test with canonicalized string literal
		log.Printf("[getRealClientIP TRACE V4] Direct map access headers[Canonical(\"CF-Connecting-IP\")] exists. Value: %v. len: %d", val, len(val))
	} else {
		log.Printf("[getRealClientIP TRACE V4] Direct map access headers[Canonical(\"CF-Connecting-IP\")] DOES NOT exist.")
	}

	canonicalRealIPHeaderName := textproto.CanonicalMIMEHeaderKey(realIPHeaderName)
	log.Printf("[getRealClientIP TRACE V4] Canonical form of realIPHeaderName ('%s') is: '%s' (len: %d)", realIPHeaderName, canonicalRealIPHeaderName, len(canonicalRealIPHeaderName))
	log.Printf("[getRealClientIP TRACE V4] Canonical form of string literal \"CF-Connecting-IP\" is: '%s'", textproto.CanonicalMIMEHeaderKey("CF-Connecting-IP"))

	headerValueForDebug := headers.Get(realIPHeaderName)
	log.Printf("[getRealClientIP TRACE V4] Value from headers.Get(realIPHeaderName) (realIPHeaderName='%s') for debug: '%s'", realIPHeaderName, headerValueForDebug)

	directPeerIP := remoteAddr
	host, _, err := net.SplitHostPort(remoteAddr)
	if err == nil {
		directPeerIP = host
	} else {
		ipAddr := net.ParseIP(remoteAddr)
		if ipAddr != nil {
			directPeerIP = ipAddr.String()
		}
	}
	log.Printf("[getRealClientIP TRACE V4] directPeerIP initialized to: %s", directPeerIP)

	if realIPHeaderName == "" {
		log.Printf("[getRealClientIP TRACE V4] realIPHeaderName is empty, returning directPeerIP: %s", directPeerIP)
		return directPeerIP
	}

	// Use canonicalRealIPHeaderName for the actual Get, just in case, though Get() should do this internally.
	headerValue := headers.Get(canonicalRealIPHeaderName)
	log.Printf("[getRealClientIP TRACE V4] headerValue for logic from headers.Get(canonicalRealIPHeaderName) (canonicalRealIPHeaderName='%s'): '%s'", canonicalRealIPHeaderName, headerValue)

	if headerValue == "" {
		log.Printf("[getRealClientIP TRACE V4] headerValue is empty (after trying canonical), returning directPeerIP: %s", directPeerIP)
		return directPeerIP
	}

	ipsInHeaderStrings := strings.Split(headerValue, ",")
	log.Printf("[getRealClientIP TRACE V4] ipsInHeaderStrings: %v", ipsInHeaderStrings)
	var candidateIPs []net.IP

	for idx, ipStr := range ipsInHeaderStrings {
		trimmedIPStr := strings.TrimSpace(ipStr)
		log.Printf("[getRealClientIP TRACE V4] Processing ipsInHeaderStrings[%d]: '%s' (trimmed: '%s')", idx, ipStr, trimmedIPStr)
		if trimmedIPStr == "" {
			log.Printf("[getRealClientIP TRACE V4] Trimmed IP string is empty, skipping.")
			continue
		}
		ip := net.ParseIP(trimmedIPStr)
		if ip == nil {
			log.Printf("[getRealClientIP TRACE V4] Malformed IP '%s', returning directPeerIP: %s", trimmedIPStr, directPeerIP)
			return directPeerIP
		}
		log.Printf("[getRealClientIP TRACE V4] Parsed IP: %s", ip.String())
		candidateIPs = append(candidateIPs, ip)
	}
	log.Printf("[getRealClientIP TRACE V4] candidateIPs: %v", candidateIPs)

	for i := len(candidateIPs) - 1; i >= 0; i-- {
		ip := candidateIPs[i]
		log.Printf("[getRealClientIP TRACE V4] Checking candidateIPs[%d]: %s", i, ip.String())
		trusted := isIPTrusted(ip, trustedProxies)
		log.Printf("[getRealClientIP TRACE V4] isIPTrusted(%s) result: %v", ip.String(), trusted)
		if !trusted {
			log.Printf("[getRealClientIP TRACE V4] IP %s is NOT trusted. Returning it.", ip.String())
			return ip.String()
		}
		log.Printf("[getRealClientIP TRACE V4] IP %s IS trusted. Continuing loop.", ip.String())
	}

	log.Printf("[getRealClientIP TRACE V4] All IPs in header were trusted or header empty of non-trusted IPs. Returning directPeerIP: %s", directPeerIP)
	return directPeerIP
}

// writeLogLine ensures the log line is written atomically using the logger's mutex.
func (al *AccessLogger) writeLogLine(line string) {
	al.mu.Lock()
	defer al.mu.Unlock()
	// al.logger is a standard log.Logger, which guarantees that each call
	// to Println (which calls Output, then Write) is atomic with respect
	// to other calls to the same logger instance.
	// Our explicit lock here provides an additional layer of safety,
	// particularly if the underlying al.output writer itself wasn't inherently thread-safe
	// or if we were performing multiple operations on al.logger that needed to be atomic as a group.
	al.logger.Println(line)
}

// LogAccess constructs and writes an access log entry.

func (al *AccessLogger) LogAccess(
	req *http.Request,
	streamID uint32,
	status int,
	responseBytes int64,
	duration time.Duration,
) {

	if al == nil {
		return
	}
	// Check the Enabled flag from its own config
	// If Enabled is nil, it defaults to true (logging proceeds).
	// If Enabled is true, logging proceeds.
	// If Enabled is false, return.
	if al.config.Enabled != nil && !*al.config.Enabled {
		return
	}
	// If logger is nil (e.g., not fully initialized, or if config.Enabled was false
	// during NewLogger and this method is somehow called directly), also return.
	if al.logger == nil {
		return
	}

	// Determine remote_addr and remote_port
	remoteAddrFull := req.RemoteAddr
	clientHost, clientPortStr, errSplit := net.SplitHostPort(remoteAddrFull)
	if errSplit != nil {
		// Could be just an IP, or malformed (e.g. Unix socket path).
		// For logging, we'll try to use remoteAddrFull as IP if it's not splitable.
		clientHost = remoteAddrFull // Use full address as host part if split fails
		clientPortStr = "0"         // Default port if not parsable
	}

	realIPHeaderName := ""
	if al.config.RealIPHeader != nil {
		realIPHeaderName = *al.config.RealIPHeader
	}
	// Note: getRealClientIP expects the host part of remoteAddr, not host:port.
	resolvedRemoteAddr := getRealClientIP(clientHost, req.Header, realIPHeaderName, al.parsedProxies)

	clientPort := 0 // Default if parsing fails
	parsedPort, errParsePort := strconv.Atoi(clientPortStr)
	if errParsePort == nil {
		clientPort = parsedPort
	} else if clientPortStr != "0" && clientPortStr != "" {
		// Silently use 0 if port parsing fails.
		// This could be logged to an error log if it's considered important.
	}

	entry := AccessLogEntry{
		Timestamp:     getTimestamp(),
		RemoteAddr:    resolvedRemoteAddr,
		RemotePort:    clientPort,
		Protocol:      req.Proto,
		Method:        req.Method,
		URI:           req.RequestURI, // Includes path and query
		Status:        status,
		ResponseBytes: responseBytes,
		DurationMs:    duration.Milliseconds(),
		H2StreamID:    streamID,
		UserAgent:     req.UserAgent(), // omitempty handles if empty
		Referer:       req.Referer(),   // omitempty handles if empty
	}

	// Config validation ensures al.config.Format is "json" if access log is enabled.
	jsonData, err := json.Marshal(entry)
	if err != nil {
		// Fallback: Log a JSON-formatted error about the marshalling failure.
		// This internal error message will go to the access log's target.
		errorMsg := fmt.Sprintf("{\"level\":\"ERROR\", \"ts\":\"%s\", \"msg\":\"Error marshalling access log entry to JSON\", \"error\":\"%s\"}",
			getTimestamp(), strings.ReplaceAll(err.Error(), "\"", "'")) // Basic sanitization for error string in JSON
		al.writeLogLine(errorMsg)
		return
	}
	al.writeLogLine(string(jsonData))
}

// Reopen closes and reopens the log file if the target is a file.
// This is used for log rotation. It is thread-safe.
func (al *AccessLogger) Reopen() error {
	if al == nil {
		return nil // Not configured
	}
	al.mu.Lock()
	defer al.mu.Unlock()

	if !config.IsFilePath(*al.config.Target) {
		return nil // Not a file target
	}

	currentFile, ok := al.output.(*os.File)
	if ok {
		// Only try to close if it's a file we manage (not stdout/stderr)
		if currentFile != os.Stdout && currentFile != os.Stderr {
			filePath := currentFile.Name() // Get path from existing file for logging
			if err := currentFile.Close(); err != nil {
				log.Printf("Error closing access log file %s during reopen: %v", filePath, err)
				// Continue to attempt reopening
			}
		}
	}

	// Use the configured target path for reopening
	filePathToOpen := *al.config.Target
	newFile, errOpen := os.OpenFile(filePathToOpen, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if errOpen != nil {
		log.Printf("Failed to reopen access log file %s: %v. Access logging may be impaired and will fall back to stdout.", filePathToOpen, errOpen)
		// Fallback to stdout
		al.logger.SetOutput(os.Stdout)
		al.output = os.Stdout
		return fmt.Errorf("failed to reopen access log file %s: %w", filePathToOpen, errOpen)
	}

	al.logger.SetOutput(newFile)
	al.output = newFile
	log.Printf("Successfully reopened access log file: %s", filePathToOpen)
	return nil
}

// Helper to map config.LogLevel to an internal severity level if needed, or just use it directly.
func getSeverity(level config.LogLevel) int {
	switch level {
	case config.LogLevelDebug:
		return 0
	case config.LogLevelInfo:
		return 1
	case config.LogLevelWarning:
		return 2
	case config.LogLevelError:
		return 3
	default:
		return 1 // Default to INFO
	}
}

// getLogLevelSeverity converts config.LogLevel to an internal integer representation for comparison.
// Lower numbers are less severe.
func getLogLevelSeverity(level config.LogLevel) int {
	switch level {
	case config.LogLevelDebug:
		return 0
	case config.LogLevelInfo:
		return 1
	case config.LogLevelWarning:
		return 2
	case config.LogLevelError:
		return 3
	default:
		// This case should ideally not be reached if config validation is robust.
		// Default to a known level (e.g., INFO) or a specific value indicating unknown.
		return 1 // Default to INFO's severity
	}
}

// isLoggable checks if the given messageLevel is severe enough to be logged
// based on the ErrorLogger's configured globalLogLevel.
func (el *ErrorLogger) isLoggable(messageLevel config.LogLevel) bool {
	if el == nil { // Should not happen if logger is properly initialized
		return false
	}
	// We log if messageLevel's severity is >= globalLogLevel's severity.
	messageSeverity := getLogLevelSeverity(messageLevel)
	configuredSeverity := getLogLevelSeverity(el.globalLogLevel)
	return messageSeverity >= configuredSeverity
}

// Reopen closes and reopens the log file if the target is a file.
// This is used for log rotation. It is thread-safe.
func (el *ErrorLogger) Reopen() error {
	if el == nil {
		return nil // Not configured
	}
	el.mu.Lock()
	defer el.mu.Unlock()

	if !config.IsFilePath(*el.config.Target) {
		return nil // Not a file target
	}

	currentFile, ok := el.output.(*os.File)
	if ok {
		// Only try to close if it's a file we manage (not stdout/stderr)
		if currentFile != os.Stdout && currentFile != os.Stderr {
			filePath := currentFile.Name() // Get path from existing file for logging
			if err := currentFile.Close(); err != nil {
				log.Printf("Error closing error log file %s during reopen: %v", filePath, err)
				// Continue to attempt reopening
			}
		}
	}

	// Use the configured target path for reopening
	filePathToOpen := *el.config.Target
	newFile, errOpen := os.OpenFile(filePathToOpen, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if errOpen != nil {
		log.Printf("Failed to reopen error log file %s: %v. Error logging may be impaired and will fall back to stderr.", filePathToOpen, errOpen)
		// Fallback to stderr
		el.logger.SetOutput(os.Stderr)
		el.output = os.Stderr
		return fmt.Errorf("failed to reopen error log file %s: %w", filePathToOpen, errOpen)
	}

	el.logger.SetOutput(newFile)
	el.output = newFile
	log.Printf("Successfully reopened error log file: %s", filePathToOpen)
	return nil
}

// Debug logs a message at DEBUG level.
func (el *ErrorLogger) Debug(msg string, context LogFields) {
	level := config.LogLevelDebug
	if context == nil {
		context = make(LogFields)
	}
	if _, ok := context["source"]; !ok {
		// runtime.Caller(1) will give the caller of this Debug method.
		_, file, line, ok := runtime.Caller(1)
		if ok {
			context["source"] = fmt.Sprintf("%s:%d", filepath.Base(file), line)
		}
	}
	el.LogError(level, msg, context)
}

// Info logs a message at INFO level.
func (el *ErrorLogger) Info(msg string, context LogFields) {
	level := config.LogLevelInfo
	if context == nil {
		context = make(LogFields)
	}
	if _, ok := context["source"]; !ok {
		_, file, line, ok := runtime.Caller(1)
		if ok {
			context["source"] = fmt.Sprintf("%s:%d", filepath.Base(file), line)
		}
	}
	el.LogError(level, msg, context)
}

// Warn logs a message at WARNING level.
func (el *ErrorLogger) Warn(msg string, context LogFields) {
	level := config.LogLevelWarning
	if context == nil {
		context = make(LogFields)
	}
	if _, ok := context["source"]; !ok {
		_, file, line, ok := runtime.Caller(1)
		if ok {
			context["source"] = fmt.Sprintf("%s:%d", filepath.Base(file), line)
		}
	}
	el.LogError(level, msg, context)
}

// Error logs a message at ERROR level.
func (el *ErrorLogger) Error(msg string, context LogFields) {
	level := config.LogLevelError
	if context == nil {
		context = make(LogFields)
	}
	if _, ok := context["source"]; !ok {
		_, file, line, ok := runtime.Caller(1)
		if ok {
			context["source"] = fmt.Sprintf("%s:%d", filepath.Base(file), line)
		}
	}
	el.LogError(level, msg, context)
}

// writeLogLine ensures the log line is written atomically using the logger's mutex.
func (el *ErrorLogger) writeLogLine(line string) {
	el.mu.Lock()
	defer el.mu.Unlock()
	// el.logger is a standard log.Logger, which guarantees that each call
	// to Println (which calls Output, then Write) is atomic with respect
	// to other calls to the same logger instance.
	// Our explicit lock here provides an additional layer of safety.
	el.logger.Println(line)
}

// LogError constructs and writes an error log entry.
// The 'context' map can contain specific keys like "source", "method", "uri", "h2_stream_id"
// which will be mapped to the corresponding fields in ErrorLogEntry.
// Other keys in 'context' will be placed in the 'Details' field of ErrorLogEntry.

func (el *ErrorLogger) LogError(level config.LogLevel, msg string, context LogFields) {
	if el == nil || el.logger == nil {
		return // Error logging not configured
	}

	if !el.isLoggable(level) {
		return // Message severity is below configured threshold
	}

	entry := ErrorLogEntry{
		Timestamp: getTimestamp(),
		Level:     string(level),
		Message:   msg,
	}

	// Populate specific fields from context and gather remaining for 'Details'
	if len(context) > 0 {
		detailsMap := make(LogFields)
		for k, v := range context {
			switch k {
			case "source":
				if val, ok := v.(string); ok {
					entry.Source = val
				} else {
					detailsMap[k] = v // Keep in details if type mismatch or not string
				}
			case "method":
				if val, ok := v.(string); ok {
					entry.RequestMethod = val
				} else {
					detailsMap[k] = v
				}
			case "uri":
				if val, ok := v.(string); ok {
					entry.RequestURI = val
				} else {
					detailsMap[k] = v
				}
			case "h2_stream_id":
				switch val := v.(type) {
				case uint32:
					entry.RequestH2StreamID = val
				case int: // Common for simple integer literals
					entry.RequestH2StreamID = uint32(val)
				case int32:
					entry.RequestH2StreamID = uint32(val)
				case float64: // JSON numbers might unmarshal as float64
					entry.RequestH2StreamID = uint32(val)
				default:
					detailsMap[k] = v // Type mismatch, keep in details
				}
			default:
				detailsMap[k] = v
			}
		}
		if len(detailsMap) > 0 {
			entry.Details = detailsMap
		}
	}

	jsonData, err := json.Marshal(entry)
	if err != nil {
		// Fallback: Log a JSON-formatted error about the marshalling failure.
		errorMsg := fmt.Sprintf("{\"level\":\"ERROR\", \"ts\":\"%s\", \"msg\":\"Error marshalling error log entry to JSON\", \"original_level\":\"%s\", \"original_msg\":\"%s\", \"error\":\"%s\"}",
			getTimestamp(), level, strings.ReplaceAll(msg, "\"", "'"), strings.ReplaceAll(err.Error(), "\"", "'")) // Basic sanitization
		el.writeLogLine(errorMsg)
		return
	}
	el.writeLogLine(string(jsonData))
}

// Convenience methods on the main Logger

// Convenience methods on the main Logger
func (l *Logger) Info(msg string, context LogFields) {
	if l.errorLog != nil {
		l.errorLog.Info(msg, context)
	}
}

func (l *Logger) Error(msg string, context LogFields) {
	if l.errorLog != nil {
		l.errorLog.Error(msg, context)
	}
}

func (l *Logger) Debug(msg string, context LogFields) {
	if l.errorLog != nil {
		l.errorLog.Debug(msg, context)
	}
}

func (l *Logger) Warn(msg string, context LogFields) {
	if l.errorLog != nil {
		l.errorLog.Warn(msg, context)
	}
}

func (l *Logger) Access(req *http.Request, streamID uint32, status int, responseBytes int64, duration time.Duration) {
	if l.accessLog != nil {
		l.accessLog.LogAccess(req, streamID, status, responseBytes, duration)
	}
}

// CloseLogFiles closes any open log files.
// This would be called during server shutdown.

func (l *Logger) CloseLogFiles() error {
	var firstErr error
	var closeErrs []string

	if l.accessLog != nil && l.accessLog.output != nil {
		// Only close if it's a file; check Target pointer first
		if l.accessLog.config.Target != nil && config.IsFilePath(*l.accessLog.config.Target) {
			if err := l.accessLog.output.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
				errMsg := fmt.Sprintf("Failed to close access log file %s: %v", *l.accessLog.config.Target, err)
				l.Error(errMsg, LogFields{"target": *l.accessLog.config.Target, "error": err.Error()})
				if firstErr == nil {
					firstErr = fmt.Errorf("closing access log '%s': %w", *l.accessLog.config.Target, err)
				}
				closeErrs = append(closeErrs, errMsg)
			}
		}
	}

	if l.errorLog != nil && l.errorLog.output != nil {
		// Only close if it's a file; check Target pointer first
		if l.errorLog.config.Target != nil && config.IsFilePath(*l.errorLog.config.Target) {
			if err := l.errorLog.output.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
				// Use a more primitive logger here if the main one is compromised
				errMsg := fmt.Sprintf("Failed to close error log file %s: %v", *l.errorLog.config.Target, err)
				fmt.Fprintf(os.Stderr, "[ERRORLOGGER] %s\n", errMsg)
				if firstErr == nil {
					firstErr = fmt.Errorf("closing error log '%s': %w", *l.errorLog.config.Target, err)
				}
				closeErrs = append(closeErrs, errMsg)
			}
		}
	}

	if len(closeErrs) > 0 {
		// If firstErr is still nil but there were errors, synthesize one.
		if firstErr == nil {
			return fmt.Errorf("encountered errors while closing log files: %s", strings.Join(closeErrs, "; "))
		}
	}
	return firstErr
}

// ReopenLogFiles is intended for SIGHUP handling.
// TODO: Implement this to close and reopen file-based log targets.
func (l *Logger) ReopenLogFiles() error {
	l.errorLog.mu.Lock()
	defer l.errorLog.mu.Unlock()
	if l.errorLog != nil && config.IsFilePath(*l.errorLog.config.Target) {
		if f, ok := l.errorLog.output.(*os.File); ok {
			if f != os.Stdout && f != os.Stderr { // Don't try to reopen stdio
				filePath := f.Name() // Get path from existing file
				if err := f.Close(); err != nil {
					// Log to stderr as a fallback if reopening fails critically
					log.Printf("Error closing error log file %s during reopen: %v", filePath, err)
					// Continue to attempt reopening
				}

				newFile, errOpen := os.OpenFile(filePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
				if errOpen != nil {
					log.Printf("Failed to reopen error log file %s: %v. Logging may be impaired.", filePath, errOpen)
					// Attempt to restore logging to stderr as a last resort
					l.errorLog.logger.SetOutput(os.Stderr)
					l.errorLog.output = os.Stderr
					return fmt.Errorf("failed to reopen error log file %s: %w", filePath, errOpen)
				}
				l.errorLog.logger.SetOutput(newFile)
				l.errorLog.output = newFile
				log.Printf("Successfully reopened error log file: %s", filePath) // Log to the new file or stderr if that failed
			}
		}
	}

	l.accessLog.mu.Lock()
	defer l.accessLog.mu.Unlock()
	if l.accessLog != nil && config.IsFilePath(*l.accessLog.config.Target) {
		if f, ok := l.accessLog.output.(*os.File); ok {
			if f != os.Stdout && f != os.Stderr {
				filePath := f.Name()
				if err := f.Close(); err != nil {
					log.Printf("Error closing access log file %s during reopen: %v", filePath, err)
				}
				newFile, errOpen := os.OpenFile(filePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
				if errOpen != nil {
					log.Printf("Failed to reopen access log file %s: %v. Logging may be impaired.", filePath, errOpen)
					l.accessLog.logger.SetOutput(os.Stdout) // Fallback
					l.accessLog.output = os.Stdout
					return fmt.Errorf("failed to reopen access log file %s: %w", filePath, errOpen)
				}
				l.accessLog.logger.SetOutput(newFile)
				l.accessLog.output = newFile
				log.Printf("Successfully reopened access log file: %s", filePath)
			}
		}
	}
	return nil
}

// Ensure config.IsFilePath is available or reimplement logic if not directly accessible
// For now, assuming config.IsFilePath is exported from the config package.
// If not, it's: func isFilePath(target string) bool { return target != "stdout" && target != "stderr" }
