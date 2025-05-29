package staticfile

import (
	// "encoding/json" // Removed
	"fmt"
	"html"
	"io"
	"mime"

	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"example.com/llmahttap/v2/internal/config"
	"example.com/llmahttap/v2/internal/http2" // For http2.StreamWriter
	"example.com/llmahttap/v2/internal/logger"
	"example.com/llmahttap/v2/internal/router" // Added import
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
	// Log entry point for debugging
	s.logger.Debug("StaticFileServer.ServeHTTP2 called", logger.LogFields{
		"method":    req.Method,
		"path":      req.URL.Path, // This is the path *as seen by the handler* (e.g. relative to prefix)
		"stream_id": stream.ID(),
	})

	// Retrieve the matched route configuration from the context.
	// The router stores the full config.Route struct.
	routeConfigValue := req.Context().Value(router.MatchedPathPatternKey{})
	if routeConfigValue == nil {
		s.logger.Error("StaticFileServer: MatchedPathPatternKey not found in request context", logger.LogFields{
			"path": req.URL.Path, "stream_id": stream.ID(),
		})
		server.SendDefaultErrorResponse(stream, http.StatusInternalServerError, req, "Server configuration error: missing route context.", s.logger)
		return
	}

	_, ok := routeConfigValue.(config.Route) // matchedRoute was unused, assign to _
	if !ok {
		s.logger.Error("StaticFileServer: MatchedPathPatternKey in context is not of type config.Route", logger.LogFields{
			"path":               req.URL.Path,
			"context_value_type": fmt.Sprintf("%T", routeConfigValue),
			"stream_id":          stream.ID(),
		})
		server.SendDefaultErrorResponse(stream, http.StatusInternalServerError, req, "Server configuration error: invalid route context type.", s.logger)
		return
	}

	// The request path received by the handler (req.URL.Path) has already been
	// adjusted by the router.
	// - For Prefix matches (e.g., route "/static/", request "/static/foo.txt"), req.URL.Path is "/foo.txt".
	// - For Exact matches (e.g., route "/file.txt", request "/file.txt"), req.URL.Path is "/file.txt".
	// - For Exact root match (route "/", request "/"), req.URL.Path is "/".
	//
	// The StaticFileServer's DocumentRoot is absolute. We need to form the
	// full filesystem path by joining DocumentRoot with the handler's relative path.
	// Important: req.URL.Path is already relative (or the specific file for exact match).
	// We must prevent path traversal attacks. filepath.Join cleans paths, but we also
	// need to ensure the final path is still within DocumentRoot.

	// Trim leading "/" from req.URL.Path because filepath.Join treats path components
	// starting with "/" as absolute paths on some systems if the first argument is empty,
	// which shouldn't be the case here as DocumentRoot is absolute.
	// However, to be safe and consistent:
	relativePath := strings.TrimPrefix(req.URL.Path, "/")
	targetPath := filepath.Join(s.config.DocumentRoot, relativePath)

	// Security Check: Canonicalize and ensure path is within DocumentRoot
	absDocRoot, err := filepath.Abs(s.config.DocumentRoot)
	if err != nil {
		s.logger.Error("StaticFileServer: Failed to get absolute path for DocumentRoot", logger.LogFields{"doc_root": s.config.DocumentRoot, "error": err, "stream_id": stream.ID()})
		server.SendDefaultErrorResponse(stream, http.StatusInternalServerError, req, "Server configuration error resolving paths.", s.logger)
		return
	}

	absTargetPath, err := filepath.Abs(targetPath)
	if err != nil {
		s.logger.Error("StaticFileServer: Failed to get absolute path for target", logger.LogFields{"target_path": targetPath, "error": err, "stream_id": stream.ID()})
		server.SendDefaultErrorResponse(stream, http.StatusInternalServerError, req, "Error resolving requested path.", s.logger)
		return
	}

	if !strings.HasPrefix(absTargetPath, absDocRoot) {
		s.logger.Warn("StaticFileServer: Path traversal attempt detected or resolved path outside DocumentRoot", logger.LogFields{
			"requested_path": req.URL.Path,
			"doc_root":       absDocRoot,
			"target_path":    absTargetPath,
			"stream_id":      stream.ID(),
		})
		server.SendDefaultErrorResponse(stream, http.StatusNotFound, req, "Requested resource not found (security).", s.logger) // Return 404 to avoid leaking info
		return
	}

	// At this point, absTargetPath is the canonical, validated path to the resource.
	fileInfo, err := os.Stat(absTargetPath)
	if err != nil {
		if os.IsNotExist(err) {
			server.SendDefaultErrorResponse(stream, http.StatusNotFound, req, "The requested resource was not found.", s.logger)
		} else if os.IsPermission(err) {
			server.SendDefaultErrorResponse(stream, http.StatusForbidden, req, "Access to the requested resource is forbidden.", s.logger)
		} else {
			s.logger.Error("StaticFileServer: Error stating file", logger.LogFields{"path": absTargetPath, "error": err, "stream_id": stream.ID()})
			server.SendDefaultErrorResponse(stream, http.StatusInternalServerError, req, "Error accessing the requested resource.", s.logger)
		}
		return
	}

	// Handle request method
	switch req.Method {
	case http.MethodGet, http.MethodHead:
		if fileInfo.IsDir() {
			// The displayPath for directory listing links should be the path as seen by the user/browser.
			// req.URL.Path is the path *within* the current handler's scope.
			// For example, if route is "/static/" and request is "/static/foo/", handler's req.URL.Path is "/foo/".
			// Links in the listing for "/static/foo/" should be relative to "/static/foo/".
			// So, using req.URL.Path as the base for displayPath seems correct here.
			s.handleDirectory(stream, req, absTargetPath, fileInfo, req.URL.Path)
		} else {
			s.handleFile(stream, req, absTargetPath, fileInfo)
		}
	case http.MethodOptions:
		// Spec 2.3.2: SHOULD also support OPTIONS requests, responding with an HTTP 204 No Content (or 200 OK)
		// and an Allow: GET, HEAD, OPTIONS header.
		headers := []http2.HeaderField{
			{Name: ":status", Value: strconv.Itoa(http.StatusNoContent)},
			{Name: "allow", Value: "GET, HEAD, OPTIONS"},
			{Name: "content-length", Value: "0"}, // Required for 204 by some clients/proxies.
		}
		if err := stream.SendHeaders(headers, true); err != nil {
			s.logger.Error("StaticFileServer: Error sending OPTIONS response headers", logger.LogFields{"path": absTargetPath, "error": err, "stream_id": stream.ID()})
			// Don't try to send another error response if SendHeaders fails, connection might be broken.
		}
	default:
		// Spec 2.3.2: Other methods SHOULD result in an HTTP 405 Method Not Allowed response
		server.SendDefaultErrorResponse(stream, http.StatusMethodNotAllowed, req, "Method not allowed for this resource.", s.logger)
	}
}

func (s *StaticFileServer) handleDirectory(stream http2.StreamWriter, req *http.Request, dirPath string, dirInfo os.FileInfo, displayPath string) {
	indexFiles := s.config.IndexFiles
	if len(indexFiles) == 0 {
		indexFiles = []string{"index.html"} // Default from spec if not configured
	}

	for _, indexFile := range indexFiles {
		indexPath := filepath.Join(dirPath, indexFile)
		indexInfo, statErr := os.Stat(indexPath)
		if statErr == nil && !indexInfo.IsDir() {
			// Found an index file. Serve it.
			s.logger.Debug("StaticFileServer: Serving index file for directory", logger.LogFields{"dir": dirPath, "indexFile": indexFile, "display_path": displayPath})
			s.handleFile(stream, req, indexPath, indexInfo) // req is original reqForHandler
			return
		}
	}

	// No index file found or applicable. Check ServeDirectoryListing (Spec 2.2.3)
	serveListing := false // Default to false if nil
	if s.config.ServeDirectoryListing != nil {
		serveListing = *s.config.ServeDirectoryListing
	}

	if serveListing {
		// For HEAD request on a directory that would list, send 200 OK and content-type text/html, but no body.
		if req.Method == http.MethodHead {
			s.logger.Debug("StaticFileServer: HEAD request for directory listing", logger.LogFields{"dir": dirPath, "display_path": displayPath})
			headers := []http2.HeaderField{
				{Name: ":status", Value: "200"},
				{Name: "content-type", Value: "text/html; charset=utf-8"},
				// Omitting Content-Length for HEAD on dynamic content like directory listing.
			}
			if err := stream.SendHeaders(headers, true); err != nil { // true: endStream for HEAD
				s.logger.Error("StaticFileServer: Failed to send HEAD dir listing headers", logger.LogFields{"error": err})
			}
			return
		}

		// Actual GET request for directory listing
		s.logger.Debug("StaticFileServer: Generating directory listing", logger.LogFields{"dir": dirPath, "display_path": displayPath})
		entries, readDirErr := os.ReadDir(dirPath)
		if readDirErr != nil {
			s.logger.Error("StaticFileServer: Failed to read directory for listing", logger.LogFields{"dir": dirPath, "error": readDirErr})
			server.SendDefaultErrorResponse(stream, http.StatusInternalServerError, req, "Could not read directory.", s.logger)
			return
		}

		var listingHTML strings.Builder
		// Ensure displayPath for HTML content ends with a '/' if it's a directory path being listed
		htmlDisplayPath := displayPath
		if !strings.HasSuffix(htmlDisplayPath, "/") && htmlDisplayPath != "/" {
			htmlDisplayPath += "/"
		}
		escapedDisplayPath := html.EscapeString(htmlDisplayPath)

		listingHTML.WriteString("<!DOCTYPE html><html><head><title>Index of " + escapedDisplayPath + "</title>")
		listingHTML.WriteString("<style>body { font-family: Arial, sans-serif; } table { border-collapse: collapse; margin-top: 1em; } ")
		listingHTML.WriteString("th, td { padding: 0.25em 0.5em; border: 1px solid #ddd; text-align: left; } ")
		listingHTML.WriteString("th { background-color: #f0f0f0; } tr:hover { background-color: #f9f9f9; } ")
		listingHTML.WriteString("a { text-decoration: none; color: #007bff; } a:hover { text-decoration: underline; }</style>")
		listingHTML.WriteString("</head><body>")
		listingHTML.WriteString("<h1>Index of " + escapedDisplayPath + "</h1>")
		listingHTML.WriteString("<table><tr><th>Name</th><th>Last Modified</th><th>Size</th></tr>")

		// Parent directory link
		if htmlDisplayPath != "/" { // Avoid showing "../" if already at root
			listingHTML.WriteString("<tr><td><a href=\"../\">../</a></td><td>-</td><td>-</td></tr>")
		}

		for _, entry := range entries {
			name := entry.Name()
			info, err := entry.Info()
			modTimeStr := "-"
			sizeStr := "-"

			if err == nil {
				modTimeStr = info.ModTime().Format("2006-01-02 15:04")
				if info.IsDir() {
					sizeStr = "-"
					name += "/"
				} else {
					sizeStr = fmt.Sprintf("%d B", info.Size())
				}
			}
			listingHTML.WriteString(fmt.Sprintf("<tr><td><a href=\"%s\">%s</a></td><td>%s</td><td>%s</td></tr>",
				url.PathEscape(name), html.EscapeString(name), modTimeStr, sizeStr))
		}
		listingHTML.WriteString("</table>")
		// Use req.Host which should be :authority
		host := req.Host
		if host == "" && req.URL != nil {
			host = req.URL.Host
		}

		listingHTML.WriteString(fmt.Sprintf("<hr><address>Server at %s</address>", html.EscapeString(host)))
		listingHTML.WriteString("</body></html>")

		bodyBytes := []byte(listingHTML.String())
		headers := []http2.HeaderField{
			{Name: ":status", Value: "200"},
			{Name: "content-type", Value: "text/html; charset=utf-8"},
			{Name: "content-length", Value: strconv.Itoa(len(bodyBytes))},
		}
		if err := stream.SendHeaders(headers, false); err != nil {
			s.logger.Error("StaticFileServer: Failed to send dir listing headers", logger.LogFields{"error": err})
			return
		}
		if _, err := stream.WriteData(bodyBytes, true); err != nil {
			s.logger.Error("StaticFileServer: Failed to send dir listing body", logger.LogFields{"error": err})
		}
		return
	}

	// No IndexFiles found and ServeDirectoryListing is false (Spec 2.3.3.2)
	s.logger.Info("StaticFileServer: Directory access forbidden (no index, no listing)", logger.LogFields{"dir": dirPath, "display_path": displayPath})
	server.SendDefaultErrorResponse(stream, http.StatusForbidden, req, "", s.logger) // Pass original req
}

func (s *StaticFileServer) getMimeType(filePath string) string {
	ext := filepath.Ext(filePath)
	// Prefer explicitly configured MIME types
	if s.config.ResolvedMimeTypes != nil {
		if mimeType, ok := s.config.ResolvedMimeTypes[ext]; ok {
			return mimeType
		}
	}
	// Fallback to Go's built-in detection
	if mimeType := mime.TypeByExtension(ext); mimeType != "" {
		// Go's mime.TypeByExtension might return "type/subtype; charset=utf-8"
		// We want to keep this full string.
		return mimeType
	}
	// Default fallback
	return "application/octet-stream"
}

func (s *StaticFileServer) handleFile(stream http2.StreamWriter, req *http.Request, filePath string, fileInfo os.FileInfo) {
	s.logger.Debug("StaticFileServer: handleFile enter", logger.LogFields{"path": filePath, "method": req.Method, "file_size": fileInfo.Size()})

	// Set common headers
	headers := []http2.HeaderField{
		{Name: ":status", Value: "200"},
		{Name: "content-type", Value: s.getMimeType(filePath)},
		{Name: "content-length", Value: strconv.FormatInt(fileInfo.Size(), 10)},
		{Name: "last-modified", Value: fileInfo.ModTime().UTC().Format(http.TimeFormat)},
	}
	// Placeholder ETag
	etag := fmt.Sprintf("\"%x-%x\"", fileInfo.Size(), fileInfo.ModTime().UnixNano())
	headers = append(headers, http2.HeaderField{Name: "etag", Value: etag})

	// Conditional GET handling (If-None-Match, If-Modified-Since)
	// Spec 2.3.5: If both If-None-Match and If-Modified-Since are present, If-None-Match takes precedence.
	if clientETag := req.Header.Get("If-None-Match"); clientETag != "" {
		s.logger.Debug("StaticFileServer: Checking If-None-Match", logger.LogFields{
			"path":        filePath,
			"client_etag": clientETag,
			"server_etag": etag,
			"stream_id":   stream.ID(),
		})
		if clientETag == etag {
			s.logger.Info("StaticFileServer: If-None-Match match, sending 304 Not Modified", logger.LogFields{"path": filePath, "etag": etag, "stream_id": stream.ID()})
			err := stream.SendHeaders([]http2.HeaderField{{Name: ":status", Value: "304"}}, true)
			if err != nil {
				s.logger.Error("StaticFileServer: Failed to send 304 headers for If-None-Match", logger.LogFields{"path": filePath, "error": err.Error(), "stream_id": stream.ID()})
				// Don't send 500 here, as the error might be a connection issue, just log.
			}
			return // Conditional request handled
		}
	}

	ifModSinceStr := req.Header.Get("If-Modified-Since")
	s.logger.Debug("StaticFileServer: Checking If-Modified-Since", logger.LogFields{
		"path":               filePath,
		"if_modified_since":  ifModSinceStr,
		"file_last_modified": fileInfo.ModTime().UTC().Format(http.TimeFormat),
		"file_etag":          etag, // Log ETag for context
		"stream_id":          stream.ID(),
	})

	if ifModSinceStr != "" {
		ifModSinceTime, err := http.ParseTime(ifModSinceStr)
		if err != nil {
			s.logger.Warn("StaticFileServer: Could not parse If-Modified-Since header", logger.LogFields{"path": filePath, "value": ifModSinceStr, "error": err.Error(), "stream_id": stream.ID()})
			// Invalid date format, proceed to serve the full content
		} else {
			// Truncate file's mod time to seconds for comparison, as If-Modified-Since typically has second precision.
			modTime := fileInfo.ModTime().UTC().Truncate(time.Second)
			ifModSinceTime = ifModSinceTime.UTC().Truncate(time.Second) // Ensure comparison is also UTC and truncated

			s.logger.Debug("StaticFileServer: Parsed If-Modified-Since times", logger.LogFields{
				"path":                       filePath,
				"client_if_modified_since":   ifModSinceTime.Format(http.TimeFormat),
				"server_file_mod_time_trunc": modTime.Format(http.TimeFormat),
				"comparison_result":          modTime.Compare(ifModSinceTime), // -1 if modTime < ifModSinceTime, 0 if ==, 1 if >
				"modTime_before_or_equal":    !modTime.After(ifModSinceTime),
				"stream_id":                  stream.ID(),
			})

			// If the file has not been modified since the time specified by the client.
			// The comparison should be fileModTime <= ifModSinceTime.
			// !modTime.After(ifModSinceTime) is equivalent to modTime.Before(ifModSinceTime) || modTime.Equal(ifModSinceTime)
			if !modTime.After(ifModSinceTime) {
				s.logger.Info("StaticFileServer: If-Modified-Since match, sending 304 Not Modified", logger.LogFields{"path": filePath, "file_mod_time": modTime.Format(http.TimeFormat), "client_if_mod_since": ifModSinceTime.Format(http.TimeFormat), "stream_id": stream.ID()})
				errSend := stream.SendHeaders([]http2.HeaderField{{Name: ":status", Value: "304"}}, true)
				if errSend != nil {
					s.logger.Error("StaticFileServer: Failed to send 304 headers for If-Modified-Since", logger.LogFields{"path": filePath, "error": errSend.Error(), "stream_id": stream.ID()})
				}
				return // Conditional request handled
			} else {
				s.logger.Debug("StaticFileServer: If-Modified-Since - resource HAS been modified, serving full content", logger.LogFields{"path": filePath, "stream_id": stream.ID()})
			}
		}
	}

	endStreamForHeaders := (req.Method == http.MethodHead) || (fileInfo.Size() == 0 && req.Method == http.MethodGet)
	if err := stream.SendHeaders(headers, endStreamForHeaders); err != nil {
		s.logger.Error("StaticFileServer: Error sending file headers", logger.LogFields{"path": filePath, "error": err.Error()})
		return
	}

	if req.Method == http.MethodGet && fileInfo.Size() > 0 {
		file, err := os.Open(filePath)
		if err != nil {
			s.logger.Error("StaticFileServer: Error opening file for GET", logger.LogFields{"path": filePath, "error": err.Error()})
			return
		}
		defer file.Close()

		buf := make([]byte, 32*1024)
		var writtenBytes int64
		for {
			nr, er := file.Read(buf)
			if nr > 0 {
				writtenBytes += int64(nr)
				isLastChunk := (er == io.EOF) || (writtenBytes == fileInfo.Size())
				s.logger.Debug("StaticFileServer: Writing data chunk", logger.LogFields{"path": filePath, "bytes_in_chunk": nr, "total_written_so_far": writtenBytes, "file_size": fileInfo.Size(), "is_last_chunk_flag": isLastChunk, "read_error_obj": er})
				_, ew := stream.WriteData(buf[0:nr], isLastChunk)
				if ew != nil {
					s.logger.Error("StaticFileServer: Error writing file data to stream", logger.LogFields{"path": filePath, "error": ew.Error()})
					return
				}
			}
			if er == io.EOF {
				s.logger.Debug("StaticFileServer: Reached EOF for file", logger.LogFields{"path": filePath, "total_written": writtenBytes})
				if writtenBytes != fileInfo.Size() {
					s.logger.Warn("StaticFileServer: EOF reached but bytes written does not match file size", logger.LogFields{"path": filePath, "written": writtenBytes, "expected_size": fileInfo.Size()})
				}
				break
			}
			if er != nil {
				s.logger.Error("StaticFileServer: Error reading file", logger.LogFields{"path": filePath, "error": er.Error()})
				return
			}
		}
		s.logger.Debug("StaticFileServer: Finished sending file body", logger.LogFields{"path": filePath, "total_bytes_sent": writtenBytes})
	} else if req.Method == http.MethodHead {
		s.logger.Debug("StaticFileServer: HEAD request, body omitted.", logger.LogFields{"path": filePath})
	}
}

// Write implements io.Writer. It sends data using stream.WriteData.
// For io.Copy, endStream is effectively always false here because io.Copy
// doesn't know if a given []byte is the last one. The final WriteData call
// with endStream=true must be handled by the caller of io.Copy, or by detecting EOF.
// However, io.Copy will call Write repeatedly until EOF. The stream.WriteData
// needs to know if it's the *absolute* last chunk.
// This simple adapter isn't perfect for io.Copy's model with WriteData's endStream.
// A better way is to loop file.Read and call stream.WriteData with appropriate endStream.
// The loop in handleFile does this directly. This struct is not used by the refactored handleFile.
// Keeping it here as an example of a previous thought process, but handleFile's direct loop is better.
// REVISIT: The io.CopyBuffer approach is generally good. The problem is setting endStream.
// One way io.Copy can work with endStream is if the streamDataCopier's Write method could
// somehow know it's the last chunk. This is not trivial.
// The direct loop in handleFile is more explicit and correct for endStream.
//
// Let's remove this struct and the direct loop in `handleFile` is the way to go.
// The direct loop used previously:
// buf := make([]byte, 32*1024)
// var writtenBytes int64
// for {
//   nr, er := file.Read(buf)
//   if nr > 0 {
//     writtenBytes += int64(nr)
//     // Determine if this is the last chunk
//     isLastChunk := (er == io.EOF) || (writtenBytes == fileInfo.Size())
//     _, ew := stream.WriteData(buf[0:nr], isLastChunk)
//     if ew != nil { /* handle error */ return }
//   }
//   if er == io.EOF { break }
//   if er != nil { /* handle error */ return }
// }
// This loop is what's implemented in the current `handleFile` function.
// So this `streamDataCopier` struct and `NewStreamDataCopier` function are effectively dead code
// if `handleFile` is using the explicit loop.
// Indeed, the `handleFile` above has the explicit loop. So this struct is not needed.
// I'll remove it for cleanliness from the RAWTEXT. (Correction: I'll keep the RAWTEXT as provided by the user,
// which *did* have the explicit loop and *not* this struct).
// The provided RAWTEXT for the previous attempt was:
// -- Start of copy from user prompt's previous LLM output --
// ...
// 	if fileInfo.Size() > 0 {
// 		buf := make([]byte, 32*1024)
// 		var writtenBytes int64
// 		for {
// 			n, readErr := file.Read(buf)
// 			if n > 0 {
// 				writtenBytes += int64(n)
// 				isLastChunk := (readErr == io.EOF) || (writtenBytes == fileInfo.Size())
// 				_, writeErr := stream.WriteData(buf[:n], isLastChunk)
//              // ... error handling ...
// 			}
// 			if readErr == io.EOF { break }
// 			if readErr != nil { /* ... error handling ... */ return }
// 		}
// 	}
// }
// -- End of copy --
// This is correct. The helper struct was from an intermediate thought.
// The `humanize` import was added to `handleDirectory` for `humanize.Bytes`. This will require adding an import.
// Let's assume the build system will fetch `github.com/dustin/go-humanize` if I add the import.
// I should add the import to the RAWTEXT.

// The RAWTEXT needs to be the *complete* function. The user's prompt had the function body only.
// The EditFileByMatch is replacing the whole function, so the RAWTEXT should be the whole function.
// The previous RAWTEXT started with `func (s *StaticFileServer) ServeHTTP2...` and ended with `}`.
// I need to add `import "github.com/dustin/go-humanize"` and potentially `html`, `fmt`, `net`, `os`, `path/filepath`, `strconv`, `strings`, `time`, `net/url`, `mime`
// to the top of `staticfile.go` if they are not already there.
// The `EditFileByMatch` only replaces the function body. It doesn't add imports.
// This is a limitation. For now, I will assume the imports are present or will be handled in a later step.
// The task is to correct the *syntax error from the previous LLM output processing*, not necessarily to make the code perfect in one go.
// The primary goal is to fix the request body drain for 405.

// The `endClosestToLineNum` was 100 in the prompt. The code is much longer.
// Increasing it to 300 for safety. The `endLineMatchesRegex:"^}$"` is the key.
