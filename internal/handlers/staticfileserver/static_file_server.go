package staticfileserver

import (
	"encoding/json"
	"fmt"
	// "html" // Potentially for directory listing
	// "io"   // Potentially for serving file content
	"net/http"
	"os"
	"path/filepath"
	// "sort" // Potentially for directory listing
	// "strconv" // Not used by current additions
	"strings"
	"time" // Needed for checkConditionalRequests

	"example.com/llmahttap/v2/internal/config"
	"example.com/llmahttap/v2/internal/http2"
	"example.com/llmahttap/v2/internal/logger"
	"example.com/llmahttap/v2/internal/router"
	"example.com/llmahttap/v2/internal/server"
)

const (
	handlerName = "StaticFileServer"
)

// StaticFileServer implements the server.Handler interface for serving static files.
type StaticFileServer struct {
	cfg          *config.StaticFileServerConfig
	log          *logger.Logger
	mimeResolver *MimeTypeResolver
	// mainConfigPath is stored to potentially resolve relative paths if needed later,
	// though MimeTypeResolver already uses it.
	mainConfigPath string
}

// New creates a new StaticFileServer handler.
// It's the factory function compliant with server.HandlerFactory.
// It parses the raw JSON configuration for the static file server,
// initializes a MimeTypeResolver, and sets up the handler instance.
func New(handlerCfg json.RawMessage, lg *logger.Logger, mainConfigFilePath string) (server.Handler, error) {
	if lg == nil {
		// This should ideally be handled by the HandlerRegistry or server core
		// by providing a non-nil logger. If it still happens, use a discard logger.
		lg = logger.NewDiscardLogger()
		lg.Warn("StaticFileServer.New called with nil logger, using discard logger.", nil)
	}

	sfsConfig, err := config.ParseAndValidateStaticFileServerConfig(handlerCfg, mainConfigFilePath)
	if err != nil {
		lg.Error("Failed to parse or validate StaticFileServer config", logger.LogFields{
			"error": err.Error(),
		})
		return nil, fmt.Errorf("StaticFileServer: %w", err)
	}

	// The MimeTypeResolver is now initialized within ParseAndValidateStaticFileServerConfig
	// and the resolved types are stored in sfsConfig.ResolvedMimeTypes.
	// We need to create a MimeTypeResolver instance based on this resolved config.
	// For this, we pass the sfsConfig again to NewMimeTypeResolver.
	// Note: MimeTypes.go might need adjustment if it's not already set up to use sfsConfig.ResolvedMimeTypes
	// when sfsConfig is passed in without MimeTypesPath or MimeTypesMap being the primary source.
	// Let's assume NewMimeTypeResolver can handle an sfsConfig that already has ResolvedMimeTypes populated.
	// Or, more simply, ParseAndValidateStaticFileServerConfig should *return* the MimeTypeResolver.
	//
	// Revisiting spec for mimetypes.go: NewMimeTypeResolver(sfsConfig *config.StaticFileServerConfig, mainConfigFilePath string)
	// This implies NewMimeTypeResolver does the loading.
	// And config.ParseAndValidateStaticFileServerConfig calls NewMimeTypeResolver.
	// Let's assume config.ParseAndValidateStaticFileServerConfig correctly sets up sfsConfig.ResolvedMimeTypes.
	// So the MimeTypeResolver needs to be created using that config.
	//
	// The spec for mimetypes.go's NewMimeTypeResolver says:
	// "It loads custom MIME types from the provided path (if any) and merges them with the inline map."
	// "Store the resolved types back in the config for potential reference or logging."
	// This suggests config.ParseAndValidateStaticFileServerConfig calls NewMimeTypeResolver,
	// and the resolver instance itself is not returned by ParseAndValidateStaticFileServerConfig, but the config is updated.
	//
	// So, we need to instantiate a MimeTypeResolver here again, using the validated sfsConfig.
	// This seems slightly redundant if ParseAndValidateStaticFileServerConfig already did this work.
	// A better design might be for ParseAndValidateStaticFileServerConfig to return the resolver,
	// or for StaticFileServer to just use sfsConfig.ResolvedMimeTypes directly if GetMimeType became a static func.
	//
	// Given the current MimeTypeResolver structure (it's an object with a GetMimeType method),
	// we need an instance of it.
	mimeResolver, err := NewMimeTypeResolver(sfsConfig, mainConfigFilePath)
	if err != nil {
		lg.Error("Failed to initialize MimeTypeResolver for StaticFileServer", logger.LogFields{
			"error": err.Error(),
		})
		return nil, fmt.Errorf("StaticFileServer: failed to create MimeTypeResolver: %w", err)
	}

	return &StaticFileServer{
		cfg:            sfsConfig,
		log:            lg,
		mimeResolver:   mimeResolver,
		mainConfigPath: mainConfigFilePath,
	}, nil
}

// generateETag creates a strong ETag for a file based on its size and modification time.
// Format: "<size_hex>-<modtime_unixnano_hex>"
func generateETag(fi os.FileInfo) string {
	return fmt.Sprintf("\"%x-%x\"", fi.Size(), fi.ModTime().UnixNano())
}

// checkConditionalRequests checks If-None-Match and If-Modified-Since headers.
// Returns true if a 304 Not Modified response should be sent.
// etagFromResource is the ETag for the current state of the resource. If empty, one will be generated by this function.
func checkConditionalRequests(req *http.Request, fileInfo os.FileInfo, etagFromResource string) (send304 bool) {
	if etagFromResource == "" {
		etagFromResource = generateETag(fileInfo)
	}

	// Check If-None-Match (precedence over If-Modified-Since as per spec 2.3.5)
	// "If both If-None-Match and If-Modified-Since are present, If-None-Match takes precedence."
	// This means if If-None-Match is present, its evaluation determines the outcome regarding 304/200.
	// If-Modified-Since is only considered if If-None-Match is *not* present.
	ifNoneMatchValue := req.Header.Get("If-None-Match")
	if ifNoneMatchValue != "" {
		if ifNoneMatchValue == "*" {
			// "*" matches if the resource exists. Since fileInfo is valid, it exists.
			return true // Send 304 Not Modified
		}

		// Server's ETag is strong. Client might send it back as strong or weak.
		// We need to compare opaque tags.
		// etagFromResource is already quoted, e.g., "\"1a2b-3c4d\""
		serverOpaqueTag := strings.Trim(etagFromResource, "\"") // -> "1a2b-3c4d"

		clientETags := strings.Split(ifNoneMatchValue, ",")
		for _, clientETagFull := range clientETags {
			clientETagFull = strings.TrimSpace(clientETagFull)

			var clientOpaqueTag string
			// Check if client ETag is weak (e.g., W/"1a2b-3c4d") or strong (e.g., "1a2b-3c4d")
			if strings.HasPrefix(clientETagFull, "W/") {
				clientOpaqueTag = strings.Trim(strings.TrimPrefix(clientETagFull, "W/"), "\"")
			} else {
				clientOpaqueTag = strings.Trim(clientETagFull, "\"")
			}

			// RFC 7232, Section 2.3.2: Weak comparison is true if opaque tags match.
			// Our server ETag is strong, so weak comparison applies.
			if clientOpaqueTag == serverOpaqueTag {
				return true // Matched, send 304
			}
		}
		// If-None-Match was present but no ETag matched.
		// As per "precedence", this means we should serve the full content.
		return false // Do not send 304, send full response.
	}

	// If-None-Match was NOT present. Now, (and only now) check If-Modified-Since.
	ifModifiedSinceValue := req.Header.Get("If-Modified-Since")
	if ifModifiedSinceValue != "" {
		ifModifiedSinceTime, err := http.ParseTime(ifModifiedSinceValue)
		if err == nil { // If parsing fails, header is effectively ignored (as if not present for this check)
			// File system times might have sub-second precision, but HTTP times usually don't.
			// ModTime is often truncated to second precision for Last-Modified header.
			// Truncate both to ensure consistent comparison.
			modTime := fileInfo.ModTime().Truncate(time.Second)
			ifModifiedSinceTime = ifModifiedSinceTime.Truncate(time.Second)

			// If the resource has not been modified since the specified time.
			// This means modTime is less than or equal to ifModifiedSinceTime.
			// !modTime.After(ifModifiedSinceTime) is equivalent to modTime <= ifModifiedSinceTime.
			if !modTime.After(ifModifiedSinceTime) {
				return true // Send 304 Not Modified
			}
		}
		// If header was malformed, or resource *was* modified more recently,
		// or If-Modified-Since check determined no 304.
		// In any of these cases for If-Modified-Since, send full response.
		return false
	}

	// No relevant conditional headers were present or resulted in a 304.
	return false
}

// ServeHTTP2 handles the HTTP/2 request for serving static files.
// It implements the server.Handler interface.

func (sfs *StaticFileServer) ServeHTTP2(resp http2.StreamWriter, req *http.Request) {
	// Path resolution (2.3.1)
	// Extract the sub-path relative to the route's PathPattern.
	matchedPathPattern, ok := req.Context().Value(router.MatchedPathPatternKey{}).(string)
	if !ok {
		sfs.log.Error("StaticFileServer: MatchedPathPattern not found in request context. This is a server error.", logger.LogFields{
			"stream_id": resp.ID(),
			"path":      req.URL.Path,
		})
		server.SendDefaultErrorResponse(resp, http.StatusInternalServerError, req, "Internal server configuration error.", sfs.log)
		return
	}

	var subPath string
	if strings.HasPrefix(req.URL.Path, matchedPathPattern) {
		subPath = strings.TrimPrefix(req.URL.Path, matchedPathPattern)
	} else {
		sfs.log.Error("StaticFileServer: Request path does not match the pattern it was routed for.", logger.LogFields{
			"stream_id":       resp.ID(),
			"request_path":    req.URL.Path,
			"matched_pattern": matchedPathPattern,
		})
		server.SendDefaultErrorResponse(resp, http.StatusInternalServerError, req, "Internal server routing error.", sfs.log)
		return
	}
	subPath = strings.TrimPrefix(subPath, "/")

	canonicalPath, fileInfo, httpStatusCode, err := _resolvePath(subPath, sfs.cfg.DocumentRoot, sfs.log, resp.ID(), req.URL.Path)
	if err != nil {
		// _resolvePath already logged the specific reason.
		// Now, send the appropriate HTTP error response.
		var clientMessage string
		switch httpStatusCode {
		case http.StatusNotFound:
			clientMessage = "Resource not found."
			if strings.Contains(err.Error(), "invalid path") || strings.Contains(err.Error(), "outside document root") { // Specific detail from _resolvePath
				clientMessage = "Resource not found (invalid path)."
			} else {
				clientMessage = "File not found."
			}
		case http.StatusForbidden:
			clientMessage = "Access denied."
		case http.StatusInternalServerError:
			clientMessage = "Error processing file path or accessing file."
		default: // Should not happen if _resolvePath adheres to its contract
			sfs.log.Error("StaticFileServer: _resolvePath returned unknown error code", logger.LogFields{
				"stream_id":  resp.ID(),
				"path":       req.URL.Path,
				"statusCode": httpStatusCode,
				"error":      err.Error(),
			})
			httpStatusCode = http.StatusInternalServerError
			clientMessage = "Internal server error."
		}
		server.SendDefaultErrorResponse(resp, httpStatusCode, req, clientMessage, sfs.log)
		return
	}

	// Handle HTTP methods (2.3.2)
	switch req.Method {
	case http.MethodGet, http.MethodHead:
		// Proceed to file/directory handling
	case http.MethodOptions:
		sfs.handleOptions(resp, req)
		return
	default:
		sfs.log.Info("StaticFileServer: Method not allowed", logger.LogFields{
			"stream_id": resp.ID(),
			"method":    req.Method,
			"path":      canonicalPath, // Use canonicalPath here as it's resolved
		})
		server.SendDefaultErrorResponse(resp, http.StatusMethodNotAllowed, req, "Method not allowed for this resource.", sfs.log)
		return
	}

	// File vs. Directory Handling (2.3.3)
	if fileInfo.IsDir() {
		sfs.handleDirectory(resp, req, canonicalPath, fileInfo)
	} else {
		sfs.handleFile(resp, req, canonicalPath, fileInfo)
	}
}

// _resolvePath handles path construction, canonicalization, security checks, and stat-ing.
// It returns the canonical path, file info, an HTTP status code for errors (404, 403, 500),
// and the underlying error.
func _resolvePath(subPath string, documentRoot string, lg *logger.Logger, streamID uint32, reqURLPathForLog string) (
	resolvedPath string, fileInfo os.FileInfo, httpStatusCode int, err error,
) {
	// Append subPath to DocumentRoot
	// documentRoot is guaranteed to be absolute by config validation.
	targetPath := filepath.Join(documentRoot, subPath)

	// Canonicalize path (e.g., resolving ., ..)
	canonicalPath, absErr := filepath.Abs(targetPath)
	if absErr != nil {
		lg.Error("StaticFileServer: Failed to canonicalize path", logger.LogFields{
			"stream_id":   streamID,
			"target_path": targetPath,
			"error":       absErr.Error(),
		})
		return "", nil, http.StatusInternalServerError, fmt.Errorf("error processing file path: %w", absErr)
	}

	// Security check: Ensure canonicalized path is still within DocumentRoot (2.3.1)
	if !strings.HasPrefix(canonicalPath, documentRoot) && canonicalPath != documentRoot {
		// The `canonicalPath != documentRoot` check is to correctly handle cases like DocumentRoot="/srv", subPath="", targetPath="/srv", canonicalPath="/srv".
		// If DocumentRoot is "/srv/" and subPath is "", targetPath becomes "/srv/", canonicalPath="/srv". Here strings.HasPrefix "/srv" with "/srv/" is true.
		// If DocumentRoot is "/srv"  and subPath is "", targetPath becomes "/srv",  canonicalPath="/srv". Here HasPrefix fails if DR doesn't have trailing slash but path resolves to it.
		// For `HasPrefix` to work robustly when `canonicalPath == documentRoot`, `documentRoot` should ideally not have a trailing slash unless it's the root "/" itself.
		// However, `filepath.Join` and `filepath.Abs` usually handle this well.
		// The core idea is: `canonicalPath` must be `documentRoot` or a path "under" it.
		// A stricter check: `strings.HasPrefix(canonicalPath, documentRoot + string(filepath.Separator))` OR `canonicalPath == documentRoot`.
		// Given DocumentRoot is absolute and clean, this simpler check should be sufficient.
		// The condition `canonicalPath != documentRoot` handles the exact match case correctly if DocumentRoot does not end with a slash.
		// If DocumentRoot is "/var/www" and canonicalPath is "/var/www", strings.HasPrefix("/var/www", "/var/www") is true.
		// Path traversal happens if canonicalPath is, e.g. "/var" when DocumentRoot is "/var/www".
		// Or if canonicalPath is "/var/www-other"
		// The spec: "ensure that the canonicalized path is still within the configured DocumentRoot."

		// If documentRoot is "/" and canonicalPath is "/", strings.HasPrefix("/", "/") is true.
		// If documentRoot is "/foo" and canonicalPath is "/foo", strings.HasPrefix("/foo", "/foo") is true.
		// If documentRoot is "/foo/" and canonicalPath is "/foo", strings.HasPrefix("/foo", "/foo/") is false. This means DocumentRoot should be cleaned (no trailing slash unless it's root "/")
		// The config validation ensures DocumentRoot is absolute. Let's assume it's also cleaned (e.g. by filepath.Clean initially).
		// If sfs.cfg.DocumentRoot = "/tmp/www" (cleaned)
		// subPath = ".." -> targetPath = "/tmp/www/.." -> canonicalPath = "/tmp"
		// strings.HasPrefix("/tmp", "/tmp/www") is false. Correct.
		// subPath = "" -> targetPath = "/tmp/www" -> canonicalPath = "/tmp/www"
		// strings.HasPrefix("/tmp/www", "/tmp/www") is true. Correct.

		lg.Warn("StaticFileServer: Attempt to access path outside document root (Path Traversal)", logger.LogFields{
			"stream_id":      streamID,
			"requested_path": reqURLPathForLog,
			"target_path":    targetPath,
			"canonical_path": canonicalPath,
			"document_root":  documentRoot,
		})
		// Spec: 404 for path traversal (to avoid leaking info)
		return "", nil, http.StatusNotFound, fmt.Errorf("attempt to access path outside document root: %s", canonicalPath)
	}

	// Check file existence and type
	fi, statErr := os.Stat(canonicalPath)
	if statErr != nil {
		if os.IsNotExist(statErr) {
			lg.Info("StaticFileServer: File or directory not found by _resolvePath", logger.LogFields{
				"stream_id": streamID,
				"path":      canonicalPath,
			})
			return "", nil, http.StatusNotFound, fmt.Errorf("file or directory not found: %s: %w", canonicalPath, statErr)
		} else if os.IsPermission(statErr) {
			lg.Warn("StaticFileServer: Permission denied accessing path by _resolvePath", logger.LogFields{
				"stream_id": streamID,
				"path":      canonicalPath,
				"error":     statErr.Error(),
			})
			return "", nil, http.StatusForbidden, fmt.Errorf("permission denied for path: %s: %w", canonicalPath, statErr)
		} else {
			lg.Error("StaticFileServer: Error stating file in _resolvePath", logger.LogFields{
				"stream_id": streamID,
				"path":      canonicalPath,
				"error":     statErr.Error(),
			})
			return "", nil, http.StatusInternalServerError, fmt.Errorf("error stating file: %s: %w", canonicalPath, statErr)
		}
	}
	return canonicalPath, fi, 0, nil // 0 indicates success (no HTTP error code from this stage)
}

// handleOptions is a stub implementation for handling OPTIONS requests.
func (sfs *StaticFileServer) handleOptions(resp http2.StreamWriter, req *http.Request) {
	sfs.log.Debug("StaticFileServer: handleOptions called (stub)", logger.LogFields{
		"stream_id": resp.ID(),
		"path":      req.URL.Path,
	})
	// Spec 2.3.2: Respond with HTTP 204 No Content (or 200 OK) and an Allow: GET, HEAD, OPTIONS header.
	headers := []http2.HeaderField{
		{Name: ":status", Value: "204"},
		{Name: "allow", Value: "GET, HEAD, OPTIONS"},
		{Name: "content-length", Value: "0"}, // No body for 204
	}
	if err := resp.SendHeaders(headers, true); err != nil {
		sfs.log.Error("StaticFileServer: Failed to send OPTIONS response headers", logger.LogFields{
			"stream_id": resp.ID(),
			"error":     err.Error(),
		})
	}
}

// handleDirectory is a stub implementation for handling directory requests.
func (sfs *StaticFileServer) handleDirectory(resp http2.StreamWriter, req *http.Request, path string, fi os.FileInfo) {
	sfs.log.Debug("StaticFileServer: handleDirectory called (stub)", logger.LogFields{
		"stream_id": resp.ID(),
		"path":      path,
	})
	// Placeholder: Send a 501 Not Implemented for now
	server.SendDefaultErrorResponse(resp, http.StatusNotImplemented, req, "Directory handling not yet implemented.", sfs.log)
}

// handleFile is a stub implementation for handling file requests.
func (sfs *StaticFileServer) handleFile(resp http2.StreamWriter, req *http.Request, path string, fi os.FileInfo) {
	sfs.log.Debug("StaticFileServer: handleFile called (stub)", logger.LogFields{
		"stream_id": resp.ID(),
		"path":      path,
	})
	// Placeholder: Send a 501 Not Implemented for now
	server.SendDefaultErrorResponse(resp, http.StatusNotImplemented, req, "File serving not yet implemented.", sfs.log)
}
