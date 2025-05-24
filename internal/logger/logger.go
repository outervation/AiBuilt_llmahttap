package logger

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
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
		if cfg.ErrorLog.Target != "stderr" {
			if cfg.ErrorLog.Target == "stdout" {
				errorOutput = os.Stdout
			} else if config.IsFilePath(cfg.ErrorLog.Target) {
				// Ensure path is absolute (validated in config)
				// TODO: Add file opening logic, SIGHUP handling will need this path
				file, errOpen := os.OpenFile(cfg.ErrorLog.Target, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
				if errOpen != nil {
					return nil, fmt.Errorf("failed to open error log file %s: %w", cfg.ErrorLog.Target, errOpen)
				}
				errorOutput = file
			} else {
				// Should not happen if config validation is correct
				return nil, fmt.Errorf("invalid error log target: %s", cfg.ErrorLog.Target)
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
		l.errorLog = &ErrorLogger{ // Default to stderr if not configured
			logger:         log.New(os.Stderr, "", 0),
			config:         config.ErrorLogConfig{Target: "stderr"}, // Minimal default
			globalLogLevel: config.LogLevelInfo,                     // Default log level
			output:         os.Stderr,
		}
	}

	// Setup Access Logger
	if cfg.AccessLog != nil && (cfg.AccessLog.Enabled == nil || *cfg.AccessLog.Enabled) {
		var accessOutput io.WriteCloser = os.Stdout // Default
		if cfg.AccessLog.Target != "stdout" {
			if cfg.AccessLog.Target == "stderr" {
				accessOutput = os.Stderr
			} else if config.IsFilePath(cfg.AccessLog.Target) {
				// Ensure path is absolute (validated in config)
				// TODO: Add file opening logic, SIGHUP handling will need this path
				file, errOpen := os.OpenFile(cfg.AccessLog.Target, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
				if errOpen != nil {
					return nil, fmt.Errorf("failed to open access log file %s: %w", cfg.AccessLog.Target, errOpen)
				}
				accessOutput = file
			} else {
				// Should not happen if config validation is correct
				return nil, fmt.Errorf("invalid access log target: %s", cfg.AccessLog.Target)
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
	var determinedDirectPeerIP string
	host, _, err := net.SplitHostPort(remoteAddr)
	if err == nil {
		// Successfully split host:port. host is the host part.
		// It could be an IP literal like "1.2.3.4" or "::1", or a hostname "localhost".
		determinedDirectPeerIP = host
	} else {
		// net.SplitHostPort failed. remoteAddr is not in "host:port" format.
		// It might be a bare IP address (e.g. "1.2.3.4", "::1"),
		// or a hostname, or a path (e.g. for Unix sockets).
		// Try to parse it as an IP. If successful, use its canonical string form.
		ip := net.ParseIP(remoteAddr)
		if ip != nil {
			determinedDirectPeerIP = ip.String() // Use canonical string representation
		} else {
			// Not a parseable IP. Use remoteAddr as is (e.g. "localhost", "[::1]" if malformed by user, path).
			determinedDirectPeerIP = remoteAddr
		}
	}

	if realIPHeaderName == "" {
		return determinedDirectPeerIP
	}

	headerValue := headers.Get(realIPHeaderName)
	if headerValue == "" {
		return determinedDirectPeerIP
	}

	// X-Forwarded-For can be "client, proxy1, proxy2"
	// We need to parse from right to left.
	ipsInHeader := strings.Split(headerValue, ",")
	for i := len(ipsInHeader) - 1; i >= 0; i-- {
		ipStr := strings.TrimSpace(ipsInHeader[i])
		if ipStr == "" { // Handle potential empty strings from "foo,,bar"
			continue
		}

		ip := net.ParseIP(ipStr)
		if ip == nil {
			// "If ... the header is malformed, the direct peer IP is used."
			// A single unparseable IP string in the list makes the header chain unreliable here.
			return determinedDirectPeerIP
		}

		if !isIPTrusted(ip, trustedProxies) {
			return ipStr // This is the first non-trusted IP from the right
		}
	}

	// If we reach here, all IPs in the header were valid and trusted,
	// or the header was effectively empty after trimming spaces.
	return determinedDirectPeerIP
}

// LogAccess constructs and writes an access log entry.
// This is a placeholder for full implementation.
func (al *AccessLogger) LogAccess(
	req *http.Request,
	streamID uint32,
	status int,
	responseBytes int64,
	duration time.Duration,
) {
	if al == nil || al.logger == nil {
		return // Access logging is disabled or not configured
	}

	// Determine remote_addr and remote_port
	remoteAddrFull := req.RemoteAddr
	_, clientPortStr, err := net.SplitHostPort(remoteAddrFull)
	if err != nil {
		// Could be just an IP, or malformed.
		// For logging, we'll try to use remoteAddrFull as IP if it's not splitable.
		clientPortStr = "0" // Or some other indicator of unknown port
	}

	realIPHeaderName := ""
	if al.config.RealIPHeader != nil {
		realIPHeaderName = *al.config.RealIPHeader
	}
	resolvedRemoteAddr := getRealClientIP(remoteAddrFull, req.Header, realIPHeaderName, al.parsedProxies)

	// Placeholder for JSON structure, adapt to spec 3.3.2
	logEntry := map[string]interface{}{
		"ts":           time.Now().UTC().Format("2006-01-02T15:04:05.000Z"), // ISO 8601 with millisecond
		"remote_addr":  resolvedRemoteAddr,
		"remote_port":  clientPortStr, // Note: This is direct peer's port.
		"protocol":     req.Proto,
		"method":       req.Method,
		"uri":          req.RequestURI,
		"status":       status,
		"resp_bytes":   responseBytes,
		"duration_ms":  duration.Milliseconds(),
		"h2_stream_id": streamID,
	}
	if ua := req.UserAgent(); ua != "" {
		logEntry["user_agent"] = ua
	}
	if ref := req.Referer(); ref != "" {
		logEntry["referer"] = ref
	}

	// TODO: Ensure atomic writes for file targets if al.mu is used.
	// For now, log.Logger handles its own synchronization for a single writer.
	// If format is "json"
	if al.config.Format == "json" {
		jsonData, err := json.Marshal(logEntry)
		if err != nil {
			// Log marshalling error to error logger?
			al.logger.Printf("Error marshalling access log entry: %v", err) // Fallback
			return
		}
		al.logger.Println(string(jsonData)) // log.Logger adds its own newline
	} else {
		// Fallback or CLF format (not specified for this stage)
		al.logger.Printf("%s %s %s %s %d %d %dms (stream %d)",
			logEntry["ts"], resolvedRemoteAddr, req.Method, req.RequestURI, status, responseBytes, duration.Milliseconds(), streamID)
	}
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

// LogError constructs and writes an error log entry.
// This is a placeholder for full implementation.
func (el *ErrorLogger) LogError(level config.LogLevel, msg string, fields ...map[string]interface{}) {
	if el == nil || el.logger == nil {
		return // Error logging not configured
	}

	// Check against global log level
	if getSeverity(level) < getSeverity(el.globalLogLevel) {
		return // Message severity is below configured threshold
	}

	// Placeholder for structured error log entry (spec 3.4.2)
	logEntry := map[string]interface{}{
		"ts":    time.Now().UTC().Format("2006-01-02T15:04:05.000Z"),
		"level": string(level),
		"msg":   msg,
	}

	// Merge additional fields
	if len(fields) > 0 {
		for k, v := range fields[0] {
			logEntry[k] = v
		}
	}

	// TODO: Add 'source' and 'associated request details' if available.
	// TODO: Ensure atomic writes for file targets.

	// For now, simple JSON output.
	jsonData, err := json.Marshal(logEntry)
	if err != nil {
		el.logger.Printf("Error marshalling error log entry: %v. Original msg: %s", err, msg)
		return
	}
	el.logger.Println(string(jsonData))
}

// Convenience methods on the main Logger
func (l *Logger) Info(msg string, fields ...map[string]interface{}) {
	if l.errorLog != nil {
		l.errorLog.LogError(config.LogLevelInfo, msg, fields...)
	}
}

func (l *Logger) Error(msg string, fields ...map[string]interface{}) {
	if l.errorLog != nil {
		l.errorLog.LogError(config.LogLevelError, msg, fields...)
	}
}

func (l *Logger) Debug(msg string, fields ...map[string]interface{}) {
	if l.errorLog != nil {
		l.errorLog.LogError(config.LogLevelDebug, msg, fields...)
	}
}

func (l *Logger) Warn(msg string, fields ...map[string]interface{}) {
	if l.errorLog != nil {
		l.errorLog.LogError(config.LogLevelWarning, msg, fields...)
	}
}

func (l *Logger) Access(req *http.Request, streamID uint32, status int, responseBytes int64, duration time.Duration) {
	if l.accessLog != nil {
		l.accessLog.LogAccess(req, streamID, status, responseBytes, duration)
	}
}

// CloseLogFiles closes any open log files.
// This would be called during server shutdown.
func (l *Logger) CloseLogFiles() {
	if l.accessLog != nil && l.accessLog.output != nil {
		if f, ok := l.accessLog.output.(*os.File); ok {
			if f != os.Stdout && f != os.Stderr {
				f.Close()
			}
		}
	}
	if l.errorLog != nil && l.errorLog.output != nil {
		if f, ok := l.errorLog.output.(*os.File); ok {
			if f != os.Stdout && f != os.Stderr {
				f.Close()
			}
		}
	}
}

// ReopenLogFiles is intended for SIGHUP handling.
// TODO: Implement this to close and reopen file-based log targets.
func (l *Logger) ReopenLogFiles() error {
	l.errorLog.mu.Lock()
	defer l.errorLog.mu.Unlock()
	if l.errorLog != nil && config.IsFilePath(l.errorLog.config.Target) {
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
	if l.accessLog != nil && config.IsFilePath(l.accessLog.config.Target) {
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
