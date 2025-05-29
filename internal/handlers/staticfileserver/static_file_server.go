package staticfileserver

import (
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

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

// ServeHTTP2 handles the HTTP/2 request for serving static files.
// It implements the server.Handler interface.
func (sfs *StaticFileServer) ServeHTTP2(resp http2.StreamWriter, req *http.Request) {
	// Path resolution (2.3.1)
	// Extract the sub-path relative to the route's PathPattern.
	// The router should pass the matched PathPattern in the request context.
	// For now, assuming req.URL.Path is the full request path.
	// We need the original matched PathPattern from the route.

	matchedPathPattern, ok := req.Context().Value(router.MatchedPathPatternKey{}).(string)
	if !ok {
		sfs.log.Error("StaticFileServer: MatchedPathPattern not found in request context. This is a server error.", logger.LogFields{
			"stream_id": resp.ID(),
			"path":      req.URL.Path,
		})
		server.SendDefaultErrorResponse(resp, http.StatusInternalServerError, req, "Internal server configuration error.", sfs.log)
		return
	}

	// The part of the request path that follows the matched PathPattern prefix.
	// Example: PathPattern: "/static/", Request Path: "/static/images/foo.png" -> subPath: "images/foo.png"
	// Example: PathPattern: "/static/", Request Path: "/static/" -> subPath: ""
	// Example: PathPattern: "/", Request Path: "/index.html" -> subPath: "index.html" (assuming MatchType: Prefix for root)
	// Example: PathPattern: "/", Request Path: "/" -> subPath: "" (assuming MatchType: Prefix for root)

	var subPath string
	if strings.HasPrefix(req.URL.Path, matchedPathPattern) {
		subPath = strings.TrimPrefix(req.URL.Path, matchedPathPattern)
	} else {
		// This case should ideally not happen if routing is correct.
		// It implies a mismatch between the router's decision and the handler's expectation.
		sfs.log.Error("StaticFileServer: Request path does not match the pattern it was routed for.", logger.LogFields{
			"stream_id":       resp.ID(),
			"request_path":    req.URL.Path,
			"matched_pattern": matchedPathPattern,
		})
		server.SendDefaultErrorResponse(resp, http.StatusInternalServerError, req, "Internal server routing error.", sfs.log)
		return
	}
	// Ensure subPath does not start with a slash if it's not empty, to prevent // in joined path.
	// filepath.Join handles this, but being explicit about the relative nature.
	subPath = strings.TrimPrefix(subPath, "/")

	// Append subPath to DocumentRoot
	// sfs.cfg.DocumentRoot is guaranteed to be absolute by config validation.
	targetPath := filepath.Join(sfs.cfg.DocumentRoot, subPath)

	// Canonicalize path (e.g., resolving ., ..)
	canonicalPath, err := filepath.Abs(targetPath)
	if err != nil {
		sfs.log.Error("StaticFileServer: Failed to canonicalize path", logger.LogFields{
			"stream_id":   resp.ID(),
			"target_path": targetPath,
			"error":       err.Error(),
		})
		server.SendDefaultErrorResponse(resp, http.StatusInternalServerError, req, "Error processing file path.", sfs.log)
		return
	}

	// Security check: Ensure canonicalized path is still within DocumentRoot (2.3.1)
	// DocumentRoot itself must be canonical for this check. It is, because it must be absolute.
	if !strings.HasPrefix(canonicalPath, sfs.cfg.DocumentRoot) {
		sfs.log.Warn("StaticFileServer: Attempt to access path outside document root (Path Traversal)", logger.LogFields{
			"stream_id":      resp.ID(),
			"requested_path": req.URL.Path,
			"target_path":    targetPath,
			"canonical_path": canonicalPath,
			"document_root":  sfs.cfg.DocumentRoot,
		})
		server.SendDefaultErrorResponse(resp, http.StatusNotFound, req, "Resource not found (invalid path).", sfs.log) // Spec: 404
		return
	}

	// Check file existence and type
	fileInfo, err := os.Stat(canonicalPath)
	if err != nil {
		if os.IsNotExist(err) {
			sfs.log.Info("StaticFileServer: File or directory not found", logger.LogFields{
				"stream_id": resp.ID(),
				"path":      canonicalPath,
			})
			server.SendDefaultErrorResponse(resp, http.StatusNotFound, req, "File not found.", sfs.log)
		} else if os.IsPermission(err) {
			sfs.log.Warn("StaticFileServer: Permission denied accessing path", logger.LogFields{
				"stream_id": resp.ID(),
				"path":      canonicalPath,
				"error":     err.Error(),
			})
			server.SendDefaultErrorResponse(resp, http.StatusForbidden, req, "Access denied.", sfs.log)
		} else {
			sfs.log.Error("StaticFileServer: Error stating file", logger.LogFields{
				"stream_id": resp.ID(),
				"path":      canonicalPath,
				"error":     err.Error(),
			})
			server.SendDefaultErrorResponse(resp, http.StatusInternalServerError, req, "Error accessing file.", sfs.log)
		}
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
			"path":      canonicalPath,
		})
		// Spec 2.3.2: "Other methods SHOULD result in an HTTP 405 Method Not Allowed response (as per Section 5)."
		// Section 5 implies content negotiation for error. server.SendDefaultErrorResponse handles that.
		// The default error response for 405 in server/errors.go correctly sets "Allow: GET, HEAD, OPTIONS".
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

func (sfs *StaticFileServer) handleOptions(resp http2.StreamWriter, req *http.Request) {
	headers := []http2.HeaderField{
		{Name: ":status", Value: strconv.Itoa(http.StatusNoContent)}, // Spec says 204 or 200. 204 is common.
		{Name: "allow", Value: "GET, HEAD, OPTIONS"},
		{Name: "content-length", Value: "0"}, // Required for 204 if not strictly prohibited by HTTP/2 for it
	}
	err := resp.SendHeaders(headers, true) // End stream with headers
	if err != nil {
		sfs.log.Error("StaticFileServer: Failed to send OPTIONS response headers", logger.LogFields{
			"stream_id": resp.ID(),
			"path":      req.URL.Path,
			"error":     err.Error(),
		})
	}
	sfs.log.Access(req, resp.ID(), http.StatusNoContent, 0, 0) // TODO: Proper duration
}

func (sfs *StaticFileServer) handleDirectory(resp http2.StreamWriter, req *http.Request, dirPath string, dirInfo os.FileInfo) {
	// Attempt to serve an IndexFile (2.3.3.2 item 1)
	// IndexFiles defaults to ["index.html"] if not provided in config.
	indexFiles := sfs.cfg.IndexFiles
	if len(indexFiles) == 0 { // Should be defaulted by config parser
		indexFiles = []string{"index.html"}
	}

	for _, indexName := range indexFiles {
		indexPath := filepath.Join(dirPath, indexName)
		indexInfo, err := os.Stat(indexPath)
		if err == nil && !indexInfo.IsDir() {
			// Found an index file, serve it as a regular file.
			// Security check: ensure index file is still within DocumentRoot.
			// This is implicitly true if dirPath was validated and indexName doesn't contain '..'.
			// But to be absolutely sure:
			absIndexPath, _ := filepath.Abs(indexPath)
			if !strings.HasPrefix(absIndexPath, sfs.cfg.DocumentRoot) {
				sfs.log.Warn("StaticFileServer: Index file path escaped document root, potential misconfiguration or attack", logger.LogFields{
					"stream_id":  resp.ID(),
					"index_path": absIndexPath,
					"doc_root":   sfs.cfg.DocumentRoot,
				})
				continue // Try next index file or fail
			}
			sfs.log.Info("StaticFileServer: Serving index file for directory", logger.LogFields{
				"stream_id":  resp.ID(),
				"dir_path":   dirPath,
				"index_file": indexPath,
			})
			sfs.handleFile(resp, req, indexPath, indexInfo)
			return
		}
	}

	// No IndexFile found. Check ServeDirectoryListing (2.2.3, 2.3.3.2 item 2 & 3)
	serveListing := false
	if sfs.cfg.ServeDirectoryListing != nil {
		serveListing = *sfs.cfg.ServeDirectoryListing
	}

	if serveListing {
		sfs.serveDirectoryListing(resp, req, dirPath)
	} else {
		sfs.log.Info("StaticFileServer: Directory listing denied and no index file found", logger.LogFields{
			"stream_id": resp.ID(),
			"path":      dirPath,
		})
		server.SendDefaultErrorResponse(resp, http.StatusForbidden, req, "Directory listing is not enabled.", sfs.log)
	}
}

func (sfs *StaticFileServer) serveDirectoryListing(resp http2.StreamWriter, req *http.Request, dirPath string) {
	sfs.log.Info("StaticFileServer: Generating directory listing", logger.LogFields{
		"stream_id": resp.ID(),
		"path":      dirPath,
	})

	entries, err := os.ReadDir(dirPath)
	if err != nil {
		sfs.log.Error("StaticFileServer: Failed to read directory for listing", logger.LogFields{
			"stream_id": resp.ID(),
			"path":      dirPath,
			"error":     err.Error(),
		})
		server.SendDefaultErrorResponse(resp, http.StatusInternalServerError, req, "Could not read directory contents.", sfs.log)
		return
	}

	// Sort entries: directories first, then by name
	sort.Slice(entries, func(i, j int) bool {
		infoI, errI := entries[i].Info()
		infoJ, errJ := entries[j].Info()
		if errI != nil || errJ != nil {
			// Handle error or sort by name if info not available
			return entries[i].Name() < entries[j].Name()
		}
		if infoI.IsDir() != infoJ.IsDir() {
			return infoI.IsDir() // Directories first (true > false)
		}
		return entries[i].Name() < entries[j].Name()
	})

	var htmlBuilder strings.Builder
	// Use request path for display, it's more user-friendly than filesystem path.
	displayPath := req.URL.Path
	htmlBuilder.WriteString("<!DOCTYPE html>\n<html>\n<head>\n")
	htmlBuilder.WriteString("<meta charset=\"utf-8\">\n")
	htmlBuilder.WriteString("<title>Index of " + html.EscapeString(displayPath) + "</title>\n")
	// Basic styling
	htmlBuilder.WriteString("<style>\n")
	htmlBuilder.WriteString("body { font-family: Arial, sans-serif; margin: 20px; }\n")
	htmlBuilder.WriteString("table { border-collapse: collapse; width: auto; min-width: 600px; }\n")
	htmlBuilder.WriteString("th, td { padding: 8px 12px; text-align: left; border-bottom: 1px solid #ddd; }\n")
	htmlBuilder.WriteString("th { background-color: #f2f2f2; }\n")
	htmlBuilder.WriteString("a { text-decoration: none; color: #0066cc; }\n")
	htmlBuilder.WriteString("a:hover { text-decoration: underline; }\n")
	htmlBuilder.WriteString("tr:hover { background-color: #f9f9f9; }\n")
	htmlBuilder.WriteString(".size { text-align: right; }\n")
	htmlBuilder.WriteString(".date { white-space: nowrap; }\n")
	htmlBuilder.WriteString("</style>\n")
	htmlBuilder.WriteString("</head>\n<body>\n")
	htmlBuilder.WriteString("<h1>Index of " + html.EscapeString(displayPath) + "</h1>\n")
	htmlBuilder.WriteString("<table>\n")
	htmlBuilder.WriteString("<tr><th>Name</th><th class=\"size\">Size</th><th class=\"date\">Last Modified</th></tr>\n")

	// Parent directory link, if not at the root of the served portion
	// req.URL.Path should end with "/" for directories if matched by Prefix.
	// The subPath calculation handles this. We need to check if `subPath` is empty for the root of the PathPattern.
	// A simpler check: if displayPath is not "/", add "../"
	if displayPath != "/" { // This logic might need refinement based on how PathPattern and subPath interact for root listings.
		// Assuming displayPath is correctly representing the URL path for the directory being listed.
		parentPath := filepath.Dir(strings.TrimSuffix(displayPath, "/"))
		if parentPath == "." {
			parentPath = "/"
		} // If original path was just "/foo/", dir is "/foo", result is "."
		if !strings.HasSuffix(parentPath, "/") {
			parentPath += "/"
		}
		htmlBuilder.WriteString("<tr><td><a href=\"" + html.EscapeString(parentPath) + "\">../</a></td><td></td><td></td></tr>\n")
	}

	for _, entry := range entries {
		name := entry.Name()
		info, err := entry.Info() // We need Stat info for size and modtime
		if err != nil {
			// Skip if we can't get info, or log
			sfs.log.Warn("StaticFileServer: Could not get FileInfo for directory entry", logger.LogFields{
				"stream_id":  resp.ID(),
				"parent_dir": dirPath,
				"entry_name": name,
				"error":      err.Error(),
			})
			continue
		}

		linkName := name
		if info.IsDir() {
			linkName += "/"
		}
		htmlBuilder.WriteString("<tr><td><a href=\"" + html.EscapeString(linkName) + "\">" + html.EscapeString(name) + "</a></td>")

		if info.IsDir() {
			htmlBuilder.WriteString("<td class=\"size\">-</td>")
		} else {
			htmlBuilder.WriteString("<td class=\"size\">" + formatFileSize(info.Size()) + "</td>")
		}
		htmlBuilder.WriteString("<td class=\"date\">" + info.ModTime().Format("2006-01-02 15:04:05 MST") + "</td></tr>\n")
	}

	htmlBuilder.WriteString("</table>\n</body>\n</html>")

	bodyBytes := []byte(htmlBuilder.String())

	headers := []http2.HeaderField{
		{Name: ":status", Value: strconv.Itoa(http.StatusOK)},
		{Name: "content-type", Value: "text/html; charset=utf-8"},
		{Name: "content-length", Value: strconv.Itoa(len(bodyBytes))},
		// Add caching headers? Directory listings are usually not heavily cached.
		{Name: "cache-control", Value: "no-cache, no-store, must-revalidate"},
	}

	if err := resp.SendHeaders(headers, false); err != nil {
		sfs.log.Error("StaticFileServer: Failed to send directory listing headers", logger.LogFields{"stream_id": resp.ID(), "path": dirPath, "error": err.Error()})
		return
	}
	if _, err := resp.WriteData(bodyBytes, true); err != nil {
		sfs.log.Error("StaticFileServer: Failed to send directory listing body", logger.LogFields{"stream_id": resp.ID(), "path": dirPath, "error": err.Error()})
	}
	sfs.log.Access(req, resp.ID(), http.StatusOK, int64(len(bodyBytes)), 0) // TODO: Proper duration
}

// formatFileSize converts bytes to a human-readable string.
func formatFileSize(sizeBytes int64) string {
	const unit = 1024
	if sizeBytes < unit {
		return fmt.Sprintf("%d B", sizeBytes)
	}
	div, exp := int64(unit), 0
	for n := sizeBytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(sizeBytes)/float64(div), "KMGTPE"[exp])
}

func (sfs *StaticFileServer) handleFile(resp http2.StreamWriter, req *http.Request, filePath string, fileInfo os.FileInfo) {
	sfs.log.Debug("StaticFileServer: Handling file request", logger.LogFields{
		"stream_id": resp.ID(),
		"path":      filePath,
		"method":    req.Method,
	})

	// Conditional GET support (ETag, If-Modified-Since) (2.3.5)
	eTag := generateETag(fileInfo)
	lastModified := fileInfo.ModTime().UTC().Format(http.TimeFormat) // Must be UTC for If-Modified-Since comparison

	// Precedence: If-None-Match takes precedence over If-Modified-Since.
	ifMatch := req.Header.Get("If-None-Match")
	if ifMatch != "" {
		// Compare with current ETag. For simplicity, assuming strong ETag comparison.
		// A robust implementation would parse multiple ETags, handle weak ETags, etc.
		// "*" is also possible, meaning "any ETag is a match".
		// For now, direct string comparison.
		clientETags := parseETags(ifMatch)
		matched := false
		for _, clientETag := range clientETags {
			if clientETag == eTag || clientETag == "*" { // "*" matches any ETag
				matched = true
				break
			}
		}

		if matched {
			sfs.log.Debug("StaticFileServer: ETag matched (If-None-Match). Sending 304 Not Modified.", logger.LogFields{
				"stream_id":   resp.ID(),
				"path":        filePath,
				"client_etag": ifMatch,
				"server_etag": eTag,
			})
			headers := []http2.HeaderField{
				{Name: ":status", Value: strconv.Itoa(http.StatusNotModified)},
				{Name: "etag", Value: eTag},
				// Date header should be added by connection/server framework
			}
			if err := resp.SendHeaders(headers, true); err != nil {
				sfs.log.Error("StaticFileServer: Failed to send 304 headers (ETag)", logger.LogFields{"stream_id": resp.ID(), "path": filePath, "error": err.Error()})
			}
			sfs.log.Access(req, resp.ID(), http.StatusNotModified, 0, 0) // TODO: Proper duration
			return
		}
	} else {
		// If-None-Match not present or did not match, check If-Modified-Since
		ifModifiedSince := req.Header.Get("If-Modified-Since")
		if ifModifiedSince != "" {
			modifiedTime, err := time.Parse(http.TimeFormat, ifModifiedSince)
			if err == nil { // If parsing fails, ignore the header
				// Compare file's mod time (truncate to second for comparison per RFC 7232)
				// File's mod time is already UTC from lastModified.
				if !fileInfo.ModTime().Truncate(time.Second).After(modifiedTime.Truncate(time.Second)) {
					sfs.log.Debug("StaticFileServer: Not modified (If-Modified-Since). Sending 304 Not Modified.", logger.LogFields{
						"stream_id":         resp.ID(),
						"path":              filePath,
						"if_modified_since": ifModifiedSince,
						"last_modified":     lastModified,
					})
					headers := []http2.HeaderField{
						{Name: ":status", Value: strconv.Itoa(http.StatusNotModified)},
						{Name: "etag", Value: eTag}, // Recommended to send ETag even with 304 from IMS
						// Last-Modified is not strictly required by RFC7232 for 304 from IMS, but often included
						// {Name: "last-modified", Value: lastModified},
						// Date header should be added
					}
					if err := resp.SendHeaders(headers, true); err != nil {
						sfs.log.Error("StaticFileServer: Failed to send 304 headers (If-Modified-Since)", logger.LogFields{"stream_id": resp.ID(), "path": filePath, "error": err.Error()})
					}
					sfs.log.Access(req, resp.ID(), http.StatusNotModified, 0, 0) // TODO: Proper duration
					return
				}
			} else {
				sfs.log.Debug("StaticFileServer: Malformed If-Modified-Since header", logger.LogFields{"stream_id": resp.ID(), "header_val": ifModifiedSince})
			}
		}
	}

	// Open the file
	file, err := os.Open(filePath)
	if err != nil {
		if os.IsPermission(err) {
			sfs.log.Warn("StaticFileServer: Permission denied opening file", logger.LogFields{"stream_id": resp.ID(), "path": filePath, "error": err.Error()})
			server.SendDefaultErrorResponse(resp, http.StatusForbidden, req, "Access denied to file.", sfs.log)
		} else { // Includes os.IsNotExist, but that should have been caught by Stat earlier.
			sfs.log.Error("StaticFileServer: Error opening file", logger.LogFields{"stream_id": resp.ID(), "path": filePath, "error": err.Error()})
			server.SendDefaultErrorResponse(resp, http.StatusInternalServerError, req, "Could not open file.", sfs.log)
		}
		return
	}
	defer file.Close()

	// Prepare headers for 200 OK
	contentType := sfs.mimeResolver.GetMimeType(filePath)
	contentLength := strconv.FormatInt(fileInfo.Size(), 10)

	headers := []http2.HeaderField{
		{Name: ":status", Value: strconv.Itoa(http.StatusOK)},
		{Name: "content-type", Value: contentType},
		{Name: "content-length", Value: contentLength},
		{Name: "last-modified", Value: lastModified},
		{Name: "etag", Value: eTag},
		// TODO: Add other relevant headers like Accept-Ranges: bytes if range requests are supported later.
	}

	err = resp.SendHeaders(headers, req.Method == http.MethodHead || fileInfo.Size() == 0)
	if err != nil {
		sfs.log.Error("StaticFileServer: Failed to send file headers", logger.LogFields{"stream_id": resp.ID(), "path": filePath, "error": err.Error()})
		// Cannot send error response if headers already failed. Connection might be closing.
		return
	}

	// For HEAD requests or zero-byte files, body is omitted (endStream was true in SendHeaders)
	if req.Method == http.MethodHead || fileInfo.Size() == 0 {
		sfs.log.Access(req, resp.ID(), http.StatusOK, 0, 0) // TODO: Proper duration
		return
	}

	// For GET requests, send file content (2.3.3.1)
	// HTTP/2 flow control is handled by http2.Stream.WriteData -> http2.Connection.sendDataFrame
	// which uses StreamFlowControlManager.AcquireSendSpace and ConnectionFlowControlManager.AcquireSendSpace.
	// These will block if the window is exhausted.
	// io.Copy will read from `file` and write to `resp` (which is an http2.StreamWriter).
	// The http2.StreamWriter's WriteData method needs to handle breaking data into frames
	// and respecting max frame size.

	// To ensure flow control and framing are handled correctly, we cannot directly use io.Copy
	// to the http2.StreamWriter *unless* its Write method is robust for large inputs.
	// Let's assume http2.StreamWriter's WriteData method chunks appropriately.
	// We still need to call WriteData repeatedly with appropriate endStream flag.

	// Stream the file content.
	// We need a buffer for io.Copy or manual chunking.
	// Max frame size for DATA frames is relevant here, but http2.Stream.WriteData should handle it.
	// The writer for stream (http2.Stream) has a WriteData method which takes endStream.
	// We should use that.

	buffer := make([]byte, 32*1024) // 32KB buffer, reasonable for chunking
	var bytesSent int64
	totalSize := fileInfo.Size()

	for {
		n, readErr := file.Read(buffer)
		if n > 0 {
			bytesSent += int64(n)
			// Determine if this is the last chunk
			isLastChunk := (bytesSent == totalSize) || (readErr == io.EOF)

			_, writeErr := resp.WriteData(buffer[:n], isLastChunk)
			if writeErr != nil {
				sfs.log.Error("StaticFileServer: Error writing file data to stream", logger.LogFields{
					"stream_id": resp.ID(),
					"path":      filePath,
					"error":     writeErr.Error(),
				})
				// If writing fails, the stream/connection might be broken.
				// An RST_STREAM might be sent by lower layers.
				return
			}
		}

		if readErr == io.EOF {
			if bytesSent != totalSize {
				sfs.log.Warn("StaticFileServer: File size mismatch after reading to EOF", logger.LogFields{
					"stream_id":     resp.ID(),
					"path":          filePath,
					"expected_size": totalSize,
					"bytes_sent":    bytesSent,
				})
				// This implies the file changed during read or an error occurred.
				// The stream might be left in an inconsistent state if not all data was sent
				// and endStream was not true on the last successful WriteData.
				// If endStream was already sent true, then nothing more can be done here.
			}
			break // Successfully read to EOF
		}
		if readErr != nil {
			sfs.log.Error("StaticFileServer: Error reading file content", logger.LogFields{
				"stream_id": resp.ID(),
				"path":      filePath,
				"error":     readErr.Error(),
			})
			// An I/O error during read. The stream might be broken.
			// If headers were sent, we can't send a 500 error.
			// The connection layer might RST_STREAM or the client will see a truncated response.
			return
		}
	}
	sfs.log.Access(req, resp.ID(), http.StatusOK, bytesSent, 0) // TODO: Proper duration
}

// generateETag creates an ETag for a file based on its size and modification time.
// Example: "size-mtime"
func generateETag(fi os.FileInfo) string {
	// Using a simple strong ETag based on size and mtime.
	// For HTTP/2, ETags should be quoted strings.
	return fmt.Sprintf("\"%x-%x\"", fi.Size(), fi.ModTime().UnixNano())
}

// parseETags parses a comma-separated list of ETags from an If-None-Match header.
// Handles quoted and unquoted ETags, and weak ETags (W/"...").
// For simplicity in current implementation, we only care about strong, direct matches.
func parseETags(headerValue string) []string {
	var etags []string
	parts := strings.Split(headerValue, ",")
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		// Remove W/ prefix for weak ETags, and quotes for strong ETags
		// For this server, we'll treat W/"foo" and "foo" as "foo" for matching.
		// This simplifies comparison but is not strictly RFC compliant for strong vs weak.
		// For now, we're generating strong ETags like "size-mtime".
		if strings.HasPrefix(part, "W/") {
			part = part[2:]
		}
		if strings.HasPrefix(part, "\"") && strings.HasSuffix(part, "\"") && len(part) > 1 {
			part = part[1 : len(part)-1]
		}
		etags = append(etags, part)
	}
	return etags
}

// StaticFileServerHandlerFactory is the factory function for StaticFileServer.
// It adapts the New function to the server.HandlerFactory signature.
func StaticFileServerHandlerFactory(handlerCfg json.RawMessage, lg *logger.Logger, mainConfigFilePath string) (server.Handler, error) {
	return New(handlerCfg, lg, mainConfigFilePath)
}
