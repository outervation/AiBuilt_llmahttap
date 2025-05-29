package staticfileserver

import (
	"encoding/json"
	"fmt"
	// "html" // Potentially for directory listing
	// "io"   // Potentially for serving file content
	"html"
	"io" // Potentially for serving file content
	"net/http"
	"os"
	"sort"

	"github.com/dustin/go-humanize"
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
		sfs.handleDirectory(resp, req, canonicalPath, fileInfo, req.URL.Path) // Pass req.URL.Path as webPath
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

func (sfs *StaticFileServer) handleDirectory(resp http2.StreamWriter, req *http.Request, dirPath string, dirFi os.FileInfo, webPath string) {
	sfs.log.Debug("StaticFileServer: handleDirectory called", logger.LogFields{
		"stream_id": resp.ID(),
		"dirPath":   dirPath,
		"webPath":   webPath, // Use the webPath parameter
	})

	// 1. Attempt to serve an IndexFile (Spec 2.3.3.2)
	// sfs.cfg.IndexFiles is guaranteed to be non-empty by config.ParseAndValidateStaticFileServerConfig (defaults to ["index.html"])
	for _, indexFileName := range sfs.cfg.IndexFiles {
		indexPath := filepath.Join(dirPath, indexFileName)
		indexFi, err := os.Stat(indexPath)

		if err == nil && !indexFi.IsDir() { // Found an index file and it's a regular file
			sfs.log.Info("StaticFileServer: Serving index file from directory", logger.LogFields{
				"stream_id":       resp.ID(),
				"dirPath":         dirPath,
				"index_file":      indexFileName,
				"index_file_path": indexPath,
			})
			// The 'req' passed to serveFile will have its original URL.Path (the directory path).
			// serveFile uses req for Method and conditional headers (If-None-Match, If-Modified-Since),
			// which should apply to the index file being served. This is acceptable.
			sfs.serveFile(resp, req, indexPath, indexFi)
			return
		}
		// If os.Stat fails (e.g., os.IsNotExist(err)) or if it's a directory, try next index file.
	}

	// 2. No IndexFile found. Check ServeDirectoryListing (Spec 2.2.3, 2.3.3.2)
	// sfs.cfg.ServeDirectoryListing is guaranteed non-nil by config.ParseAndValidateStaticFileServerConfig
	if *sfs.cfg.ServeDirectoryListing {
		sfs.log.Info("StaticFileServer: No index file found, serving directory listing", logger.LogFields{
			"stream_id": resp.ID(),
			"dirPath":   dirPath,
			"webPath":   webPath, // Use the webPath parameter
		})

		htmlBody, err := sfs.generateDirectoryListingHTML(dirPath, webPath) // Pass webPath parameter
		if err != nil {
			sfs.log.Error("StaticFileServer: Failed to generate directory listing HTML", logger.LogFields{
				"stream_id": resp.ID(),
				"dirPath":   dirPath,
				"webPath":   webPath, // Use the webPath parameter
				"error":     err.Error(),
			})
			server.SendDefaultErrorResponse(resp, http.StatusInternalServerError, req, "Error generating directory listing.", sfs.log)
			return
		}

		headers := []http2.HeaderField{
			{Name: ":status", Value: "200"},
			{Name: "content-type", Value: "text/html; charset=utf-8"},
			{Name: "content-length", Value: fmt.Sprintf("%d", len(htmlBody))},
			// Consider "Cache-Control: no-cache" for directory listings if freshness is critical
		}

		if err := resp.SendHeaders(headers, false); err != nil { // endStream=false, body follows
			sfs.log.Error("StaticFileServer: Failed to send directory listing headers", logger.LogFields{
				"stream_id": resp.ID(),
				"dirPath":   dirPath,
				"error":     err.Error(),
			})
			return // Don't attempt to write body if headers failed
		}

		if _, err := resp.WriteData(htmlBody, true); err != nil { // endStream=true, this is the full body
			sfs.log.Error("StaticFileServer: Failed to write directory listing body", logger.LogFields{
				"stream_id": resp.ID(),
				"dirPath":   dirPath,
				"error":     err.Error(),
			})
			// Stream is likely broken; HTTP/2 layer will handle reset.
		}
		return
	}

	// 3. No IndexFile found and ServeDirectoryListing is false (Spec 2.3.3.2)
	sfs.log.Info("StaticFileServer: No index file and directory listing disabled, sending 403 Forbidden", logger.LogFields{
		"stream_id": resp.ID(),
		"dirPath":   dirPath,
	})
	server.SendDefaultErrorResponse(resp, http.StatusForbidden, req, "Access to this directory is forbidden.", sfs.log)
}

// handleFile is a stub implementation for handling file requests.

func (sfs *StaticFileServer) handleFile(resp http2.StreamWriter, req *http.Request, path string, fi os.FileInfo) {
	sfs.log.Debug("StaticFileServer: handleFile dispatching to serveFile", logger.LogFields{
		"stream_id": resp.ID(),
		"path":      path,
		"method":    req.Method,
		"is_dir":    fi.IsDir(), // Should be false if logic is correct upstream
	})

	// This check is a safeguard. Callers of handleFile (like ServeHTTP2)
	// should ensure 'fi' is not a directory.
	if fi.IsDir() {
		sfs.log.Error("StaticFileServer: handleFile called with a directory, which is a logic error.", logger.LogFields{
			"stream_id": resp.ID(),
			"path":      path,
		})
		server.SendDefaultErrorResponse(resp, http.StatusInternalServerError, req, "Internal server error: unexpected resource type.", sfs.log)
		return
	}

	sfs.serveFile(resp, req, path, fi)
}

// serveFile handles the serving of a regular file, including conditional requests,
// header generation, and streaming content for GET requests.
func (sfs *StaticFileServer) serveFile(resp http2.StreamWriter, req *http.Request, filePath string, fileInfo os.FileInfo) {
	etag := generateETag(fileInfo)
	lastModified := fileInfo.ModTime().UTC() // Ensure UTC for consistent formatting

	// Check for conditional requests (If-None-Match, If-Modified-Since)
	if checkConditionalRequests(req, fileInfo, etag) {
		headers := []http2.HeaderField{
			{Name: ":status", Value: "304"},
			{Name: "etag", Value: etag},
			{Name: "last-modified", Value: lastModified.Format(http.TimeFormat)},
			// Date header will be added by the server/connection layer
		}
		sfs.log.Debug("StaticFileServer: Sending 304 Not Modified", logger.LogFields{
			"stream_id": resp.ID(), "path": filePath, "etag": etag,
		})
		if err := resp.SendHeaders(headers, true); err != nil { // true because no body for 304
			sfs.log.Error("StaticFileServer: Failed to send 304 headers", logger.LogFields{
				"stream_id": resp.ID(), "path": filePath, "error": err.Error(),
			})
		}
		return
	}

	// Not a 304, so prepare to send the full response (200 OK)
	contentType := sfs.mimeResolver.GetMimeType(filePath)
	contentLengthStr := fmt.Sprintf("%d", fileInfo.Size())

	responseHeaders := []http2.HeaderField{
		{Name: ":status", Value: "200"},
		{Name: "content-type", Value: contentType},
		{Name: "content-length", Value: contentLengthStr},
		{Name: "last-modified", Value: lastModified.Format(http.TimeFormat)},
		{Name: "etag", Value: etag},
		// Consider adding "Accept-Ranges: bytes" if range requests were supported.
	}

	// Handle HEAD request: send headers only, then return
	if req.Method == http.MethodHead {
		sfs.log.Debug("StaticFileServer: Sending HEAD response", logger.LogFields{
			"stream_id": resp.ID(), "path": filePath, "content_type": contentType, "content_length": contentLengthStr,
		})
		if err := resp.SendHeaders(responseHeaders, true); err != nil { // true because no body for HEAD
			sfs.log.Error("StaticFileServer: Failed to send HEAD headers", logger.LogFields{
				"stream_id": resp.ID(), "path": filePath, "error": err.Error(),
			})
		}
		return
	}

	// Handle GET request
	// If file is empty, send headers with endStream=true and no body.
	if fileInfo.Size() == 0 {
		sfs.log.Debug("StaticFileServer: Sending GET response for empty file", logger.LogFields{
			"stream_id": resp.ID(), "path": filePath,
		})
		if err := resp.SendHeaders(responseHeaders, true); err != nil { // true because no body
			sfs.log.Error("StaticFileServer: Failed to send headers for empty file", logger.LogFields{
				"stream_id": resp.ID(), "path": filePath, "error": err.Error(),
			})
		}
		return
	}

	// File has content, open it for reading.
	file, err := os.Open(filePath)
	if err != nil {
		// Stat succeeded, but Open failed. This could be a permissions issue that
		// Stat didn't catch, or a race condition, or the file type changed.
		if os.IsPermission(err) {
			sfs.log.Warn("StaticFileServer: Permission denied opening file for GET", logger.LogFields{
				"stream_id": resp.ID(), "path": filePath, "error": err.Error(),
			})
			server.SendDefaultErrorResponse(resp, http.StatusForbidden, req, "Access denied while opening file.", sfs.log)
		} else {
			sfs.log.Error("StaticFileServer: Failed to open file for GET", logger.LogFields{
				"stream_id": resp.ID(), "path": filePath, "error": err.Error(),
			})
			server.SendDefaultErrorResponse(resp, http.StatusInternalServerError, req, "Error reading file.", sfs.log)
		}
		return
	}
	defer file.Close()

	// Send headers (endStream=false as data will follow)
	if err := resp.SendHeaders(responseHeaders, false); err != nil {
		sfs.log.Error("StaticFileServer: Failed to send GET headers before body", logger.LogFields{
			"stream_id": resp.ID(), "path": filePath, "error": err.Error(),
		})
		// Don't attempt to send body if headers failed
		return
	}

	sfs.log.Debug("StaticFileServer: Sending GET response with body", logger.LogFields{
		"stream_id": resp.ID(), "path": filePath, "size": fileInfo.Size(),
	})

	// Stream file content
	buffer := make([]byte, 32*1024) // 32KB buffer for reading
	for {
		bytesRead, readErr := file.Read(buffer)

		// Handle read errors first, other than EOF
		if readErr != nil && readErr != io.EOF {
			sfs.log.Error("StaticFileServer: Error reading file content", logger.LogFields{
				"stream_id": resp.ID(), "path": filePath, "error": readErr.Error(),
			})
			// Headers (200 OK) have already been sent.
			// The stream will likely be reset by the HTTP/2 layer with INTERNAL_ERROR.
			// No further error response can be sent on this stream.
			return
		}

		// If bytes were read, send them
		if bytesRead > 0 {
			// isLastChunk is true if this read operation also encountered EOF.
			isLastChunk := (readErr == io.EOF)
			_, writeErr := resp.WriteData(buffer[:bytesRead], isLastChunk)
			if writeErr != nil {
				sfs.log.Error("StaticFileServer: Error writing file data to stream", logger.LogFields{
					"stream_id": resp.ID(), "path": filePath, "error": writeErr.Error(),
				})
				// Stream is likely broken. HTTP/2 layer should handle reset.
				return
			}
		}

		// If EOF was encountered (either with bytesRead > 0 or bytesRead == 0), we're done.
		if readErr == io.EOF {
			break
		}
	}

	sfs.log.Debug("StaticFileServer: Finished streaming file", logger.LogFields{
		"stream_id": resp.ID(), "path": filePath,
	})
}

// generateDirectoryListingHTML creates an HTML page listing the contents of a directory.
// dirPath is the absolute filesystem path to the directory.
// webPath is the URL path that corresponds to this directory, used for link generation.
func (sfs *StaticFileServer) generateDirectoryListingHTML(dirPath string, webPath string) ([]byte, error) {
	sfs.log.Debug("Generating directory listing", logger.LogFields{"dirPath": dirPath, "webPath": webPath})

	entries, err := os.ReadDir(dirPath)
	if err != nil {
		sfs.log.Error("Error reading directory for listing", logger.LogFields{"dirPath": dirPath, "error": err.Error()})
		return nil, fmt.Errorf("could not read directory %s: %w", dirPath, err)
	}

	// Sort entries: directories first, then files, then by name.
	sort.Slice(entries, func(i, j int) bool {
		infoI, errI := entries[i].Info()
		infoJ, errJ := entries[j].Info()

		// Handle errors during Info() by treating them as non-directories and perhaps logging.
		// For sorting, errors might push items to the end or treat them as files.
		isDirI := false
		if errI == nil {
			isDirI = infoI.IsDir()
		} else {
			sfs.log.Warn("Error getting FileInfo for sorting directory entry", logger.LogFields{"entry": entries[i].Name(), "dirPath": dirPath, "error": errI.Error()})
		}

		isDirJ := false
		if errJ == nil {
			isDirJ = infoJ.IsDir()
		} else {
			sfs.log.Warn("Error getting FileInfo for sorting directory entry", logger.LogFields{"entry": entries[j].Name(), "dirPath": dirPath, "error": errJ.Error()})
		}

		if isDirI != isDirJ {
			return isDirI // true if i is dir and j is not (dirs first)
		}
		return strings.ToLower(entries[i].Name()) < strings.ToLower(entries[j].Name())
	})

	var sb strings.Builder
	escapedWebPath := html.EscapeString(webPath)
	sb.WriteString(fmt.Sprintf("<html><head><title>Index of %s</title></head><body>", escapedWebPath))
	sb.WriteString(fmt.Sprintf("<h1>Index of %s</h1><hr><pre>", escapedWebPath))

	// Parent directory link, if not at the root of the served path
	if webPath != "/" && webPath != "" { // webPath might be "" if it's the DocumentRoot itself matched by a "/" pattern.
		// Ensure webPath ends with a slash for proper relative linking if it's not already root.
		// However, links should be absolute from the current webPath.
		parentWebPath := filepath.Dir(strings.TrimSuffix(webPath, "/"))
		if parentWebPath == "." { // filepath.Dir of "/foo" is "/"
			parentWebPath = "/"
		}
		if !strings.HasSuffix(parentWebPath, "/") && parentWebPath != "/" {
			parentWebPath += "/"
		}
		sb.WriteString(fmt.Sprintf("<a href=\"%s\">../</a>\n", html.EscapeString(parentWebPath)))
	}

	for _, entry := range entries {
		entryName := entry.Name()
		escapedEntryName := html.EscapeString(entryName)

		// Construct the link href ensuring it's relative to the current webPath
		linkHref := escapedEntryName
		if !strings.HasSuffix(escapedWebPath, "/") && escapedWebPath != "/" && escapedWebPath != "" {
			// This case is tricky. If webPath is "/static" and entry is "foo", link should be "/static/foo"
			// If webPath is "/static/" and entry is "foo", link should be "foo" (relative to /static/) or "/static/foo"
			// Let's build absolute paths from the server root for simplicity in hrefs for now.
			hrefWebPath := strings.TrimSuffix(webPath, "/") + "/" + escapedEntryName
			linkHref = html.EscapeString(hrefWebPath)
		} else if escapedWebPath == "/" {
			linkHref = html.EscapeString("/" + escapedEntryName)
		} else { // webPath ends with "/" or is empty (implicitly root)
			linkHref = html.EscapeString(escapedWebPath + escapedEntryName)
		}

		fileInfo, err := entry.Info()
		if err != nil {
			sfs.log.Warn("Could not get info for directory entry, skipping in listing", logger.LogFields{"entry": entryName, "error": err.Error()})
			sb.WriteString(fmt.Sprintf("%s - Error getting info\n", escapedEntryName))
			continue
		}

		if fileInfo.IsDir() {
			sb.WriteString(fmt.Sprintf("<a href=\"%s/\">%s/</a>", linkHref, escapedEntryName))
			// Align size and date for directories similar to files, or leave blank
			sb.WriteString(fmt.Sprintf("%40s %20s\n", "-", fileInfo.ModTime().Format("02-Jan-2006 15:04")))
		} else {
			sb.WriteString(fmt.Sprintf("<a href=\"%s\">%s</a>", linkHref, escapedEntryName))
			// Right-align size and date. Pad name to a certain width.
			// Name (variable) | ModTime (fixed) | Size (variable, right-aligned)
			// Example from Apache: name.html             12-Oct-2023 10:00     1.2K
			// Max name length for alignment might be around 50?
			namePad := 50 - len(escapedEntryName)
			if namePad < 1 {
				namePad = 1
			}
			sb.WriteString(fmt.Sprintf("%*s %20s %10s\n",
				namePad, "", // This creates padding after the name
				fileInfo.ModTime().Format("02-Jan-2006 15:04"),
				humanize.Bytes(uint64(fileInfo.Size()))))
		}
	}

	sb.WriteString("</pre><hr></body></html>")
	return []byte(sb.String()), nil
}
