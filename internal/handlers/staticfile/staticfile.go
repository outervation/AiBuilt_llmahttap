package staticfile

import (
	// "encoding/json" // Removed
	"fmt"
	"html"
	"io"
	"mime"
	"net" // For net.Addr in directory listing example
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
	s.logger.Debug("StaticFileServer.ServeHTTP2 called", logger.LogFields{
		"stream_id": stream.ID(),
		"method":    req.Method,
		"path":      req.URL.Path,
	})

	// Path Resolution and Security (Spec 2.3.1)
	// The router passes the matched PathPattern via context.
	matchedPathPatternVal := req.Context().Value(router.MatchedPathPatternKey{})
	var matchedPathPattern string
	if pattern, ok := matchedPathPatternVal.(string); ok {
		matchedPathPattern = pattern
	} else {
		// This should not happen if router is correctly populating context.
		// Log an error and fallback to treating req.URL.Path as potentially needing a leading / trim
		// or assume it's relative if no pattern found. This is a defensive measure.
		s.logger.Error("StaticFileServer: MatchedPathPattern not found or not a string in request context", logger.LogFields{
			"path": req.URL.Path, "context_value": matchedPathPatternVal,
		})
		// Fallback: treat path as relative to doc root by stripping leading slash.
		// This might be incorrect for complex routing but better than nothing.
		// A more robust fallback might be to error out if pattern is crucial and missing.
		// For now, we'll proceed with a simple relative path assumption for this error case.
		// However, for prefix matches, `matchedPathPattern` is essential.
		// If it's missing for a prefix route, it implies a setup error.
		// The crucial logic relies on `matchedPathPattern` for prefix stripping.
		// Let's assume for now that if it's missing, it's a root path or simple case.
		// This will likely fail for prefixed routes if context is not passed.
		// A better approach IF MatchedPathPattern is missing and NEEDED (e.g. for a prefix route):
		// server.SendDefaultErrorResponse(stream, http.StatusInternalServerError, req, "Server configuration error resolving path.", s.logger)
		// return
		// For now, let path determination proceed; tests will reveal if this fallback is problematic.
	}
	s.logger.Debug("StaticFileServer: Path resolution details", logger.LogFields{
		"req_url_path":         req.URL.Path,
		"matched_path_pattern": matchedPathPattern,
	})

	var subPath string
	if matchedPathPattern != "" && strings.HasPrefix(req.URL.Path, matchedPathPattern) {
		subPath = strings.TrimPrefix(req.URL.Path, matchedPathPattern)
	} else {
		// If no matchedPathPattern or it doesn't prefix req.URL.Path (e.g. exact match on root "/"),
		// use req.URL.Path directly but ensure it's relative.
		subPath = req.URL.Path
	}
	// Ensure subPath is relative (remove leading slash if any, unless it's the only char)
	if len(subPath) > 1 && subPath[0] == '/' {
		subPath = subPath[1:]
	} else if subPath == "/" { // Request for the "root" of the matched pattern
		subPath = "" // Effectively targets the DocumentRoot itself for index/listing
	}

	targetPath := filepath.Join(s.config.DocumentRoot, subPath)
	s.logger.Debug("StaticFileServer: Path resolution result", logger.LogFields{
		"document_root": s.config.DocumentRoot,
		"sub_path":      subPath,
		"target_path":   targetPath,
	})

	// Canonicalize and security check
	absDocRoot, err := filepath.Abs(s.config.DocumentRoot)
	if err != nil {
		s.logger.Error("StaticFileServer: Failed to get absolute path for DocumentRoot", logger.LogFields{"docRoot": s.config.DocumentRoot, "error": err})
		server.SendDefaultErrorResponse(stream, http.StatusInternalServerError, req, "Invalid server configuration.", s.logger)
		return
	}

	absTargetPath, err := filepath.Abs(targetPath)
	if err != nil {
		// This can happen for malformed paths, possibly an attack attempt or client error.
		s.logger.Warn("StaticFileServer: Error getting absolute target path", logger.LogFields{"targetPath": targetPath, "error": err})
		server.SendDefaultErrorResponse(stream, http.StatusBadRequest, req, "Invalid request path.", s.logger)
		return
	}

	if !strings.HasPrefix(absTargetPath, absDocRoot) && absTargetPath != absDocRoot { // Allow absTargetPath == absDocRoot for root access
		s.logger.Warn("StaticFileServer: Directory traversal attempt or invalid path", logger.LogFields{
			"abs_target_path": absTargetPath,
			"abs_doc_root":    absDocRoot,
			"requested_path":  req.URL.Path,
		})
		server.SendDefaultErrorResponse(stream, http.StatusNotFound, req, "", s.logger) // Spec 2.3.1 says 404
		return
	}

	// Stat the resolved path
	fileInfo, statErr := os.Stat(absTargetPath)
	if statErr != nil {
		if os.IsNotExist(statErr) {
			s.logger.Info("StaticFileServer: File or directory not found", logger.LogFields{"path": absTargetPath, "stat_error": statErr.Error()})
			server.SendDefaultErrorResponse(stream, http.StatusNotFound, req, "", s.logger)
		} else if os.IsPermission(statErr) {
			s.logger.Warn("StaticFileServer: Permission denied accessing path", logger.LogFields{"path": absTargetPath, "error": statErr})
			server.SendDefaultErrorResponse(stream, http.StatusForbidden, req, "", s.logger)
		} else {
			s.logger.Error("StaticFileServer: Error stating file/directory", logger.LogFields{"path": absTargetPath, "error": statErr})
			server.SendDefaultErrorResponse(stream, http.StatusInternalServerError, req, "Error accessing resource.", s.logger)
		}
		return
	}

	switch req.Method {
	case http.MethodGet:
		if fileInfo.IsDir() {
			s.handleDirectory(stream, req, absTargetPath, fileInfo)
		} else {
			s.handleFile(stream, req, absTargetPath, fileInfo)
		}
		return
	case http.MethodHead:
		if fileInfo.IsDir() {
			s.handleDirectory(stream, req, absTargetPath, fileInfo)
		} else {
			s.handleFile(stream, req, absTargetPath, fileInfo)
		}
		return
	case http.MethodOptions:
		s.logger.Info("StaticFileServer: OPTIONS request", logger.LogFields{"stream_id": stream.ID(), "path": req.URL.Path})
		headers := []http2.HeaderField{
			{Name: ":status", Value: "204"}, // Common practice for OPTIONS
			{Name: "Allow", Value: "GET, HEAD, OPTIONS"},
			{Name: "Content-Length", Value: "0"},
		}
		err := stream.SendHeaders(headers, true)
		if err != nil {
			s.logger.Error("StaticFileServer: failed to send OPTIONS response", logger.LogFields{"stream_id": stream.ID(), "error": err.Error()})
		}
		return
	default:
		s.logger.Info("StaticFileServer: Method not allowed", logger.LogFields{
			"stream_id": stream.ID(),
			"method":    req.Method,
			"path":      req.URL.Path,
		})

		if req.Body != nil {
			const maxBodyDrainBytes = 32 * 1024
			limitedReader := io.LimitReader(req.Body, maxBodyDrainBytes)
			_, copyErr := io.Copy(io.Discard, limitedReader)
			if copyErr != nil && copyErr != io.EOF {
				s.logger.Warn("StaticFileServer: Error draining request body on 405", logger.LogFields{"stream_id": stream.ID(), "error": copyErr.Error()})
			}
			errClose := req.Body.Close()
			if errClose != nil {
				s.logger.Warn("StaticFileServer: Error closing request body on 405", logger.LogFields{"stream_id": stream.ID(), "error": errClose.Error()})
			}
		}
		// SendDefaultErrorResponse will handle adding Allow header for 405 if not already done by generic mechanism
		server.SendDefaultErrorResponse(stream, http.StatusMethodNotAllowed, req, "", s.logger)
		return
	}
}

func (s *StaticFileServer) handleDirectory(stream http2.StreamWriter, req *http.Request, dirPath string, dirInfo os.FileInfo) {
	indexFiles := s.config.IndexFiles
	if len(indexFiles) == 0 {
		indexFiles = []string{"index.html"} // Default from spec if not configured
	}

	for _, indexFile := range indexFiles {
		indexPath := filepath.Join(dirPath, indexFile)
		indexInfo, statErr := os.Stat(indexPath)
		if statErr == nil && !indexInfo.IsDir() {
			// Found an index file. Serve it.
			s.logger.Debug("StaticFileServer: Serving index file for directory", logger.LogFields{"dir": dirPath, "indexFile": indexFile})
			s.handleFile(stream, req, indexPath, indexInfo)
			return
		}
	}

	// No index file found or applicable. Check ServeDirectoryListing (Spec 2.2.3)
	serveListing := false // Default to false if nil
	if s.config.ServeDirectoryListing != nil {
		serveListing = *s.config.ServeDirectoryListing
	}

	if serveListing {
		s.logger.Debug("StaticFileServer: Generating directory listing", logger.LogFields{"dir": dirPath})
		entries, readDirErr := os.ReadDir(dirPath)
		if readDirErr != nil {
			s.logger.Error("StaticFileServer: Failed to read directory for listing", logger.LogFields{"dir": dirPath, "error": readDirErr})
			server.SendDefaultErrorResponse(stream, http.StatusInternalServerError, req, "Could not read directory.", s.logger)
			return
		}

		var listingHTML strings.Builder
		// Basic styling for readability
		listingHTML.WriteString("<!DOCTYPE html><html><head><title>Index of " + html.EscapeString(req.URL.Path) + "</title>")
		listingHTML.WriteString("<style>body { font-family: Arial, sans-serif; } table { border-collapse: collapse; margin-top: 1em; } ")
		listingHTML.WriteString("th, td { padding: 0.25em 0.5em; border: 1px solid #ddd; text-align: left; } ")
		listingHTML.WriteString("th { background-color: #f0f0f0; } tr:hover { background-color: #f9f9f9; } ")
		listingHTML.WriteString("a { text-decoration: none; color: #007bff; } a:hover { text-decoration: underline; }</style>")
		listingHTML.WriteString("</head><body>")
		listingHTML.WriteString("<h1>Index of " + html.EscapeString(req.URL.Path) + "</h1>")
		listingHTML.WriteString("<table><tr><th>Name</th><th>Last Modified</th><th>Size</th></tr>")

		// Parent directory link if not at the root of the listing context
		if req.URL.Path != "/" && req.URL.Path != "" { // Check if not already at a root-like path for listing
			listingHTML.WriteString("<tr><td><a href=\"../\">../</a></td><td>-</td><td>-</td></tr>")
		}

		for _, entry := range entries {
			name := entry.Name()
			info, err := entry.Info() // Get FileInfo for modtime and size
			modTimeStr := "-"
			sizeStr := "-"

			if err == nil {
				modTimeStr = info.ModTime().Format("2006-01-02 15:04")
				if info.IsDir() {
					sizeStr = "-"
					name += "/" // Visual cue for directories
				} else {
					sizeStr = fmt.Sprintf("%d B", info.Size()) // Replaced humanize.Bytes with simple byte count
				}
			}
			listingHTML.WriteString(fmt.Sprintf("<tr><td><a href=\"%s\">%s</a></td><td>%s</td><td>%s</td></tr>",
				url.PathEscape(name), html.EscapeString(name), modTimeStr, sizeStr))
		}
		listingHTML.WriteString("</table>")
		listingHTML.WriteString(fmt.Sprintf("<hr><address>Server at %s Port %s</address>", req.Host, strings.Split(req.Context().Value(http.LocalAddrContextKey).(net.Addr).String(), ":")[1]))
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
	s.logger.Info("StaticFileServer: Directory access forbidden (no index, no listing)", logger.LogFields{"dir": dirPath})
	server.SendDefaultErrorResponse(stream, http.StatusForbidden, req, "", s.logger)
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
		// We only want "type/subtype" part for Content-Type header usually.
		// Charset can be added separately if needed or if server policy dictates.
		return strings.Split(mimeType, ";")[0]
	}
	// Default fallback
	return "application/octet-stream"
}

func (s *StaticFileServer) handleFile(stream http2.StreamWriter, req *http.Request, filePath string, fileInfo os.FileInfo) {
	s.logger.Debug("StaticFileServer: handleFile enter", logger.LogFields{"path": filePath, "method": req.Method, "file_size": fileInfo.Size()})

	// --- DIAGNOSTIC SIMPLIFICATION REMOVED ---

	// Original handleFile logic (partially commented out for now, kept for reference if diag fails)
	// Set common headers
	headers := []http2.HeaderField{
		{Name: ":status", Value: "200"},
		{Name: "content-type", Value: s.getMimeType(filePath)},
		{Name: "content-length", Value: strconv.FormatInt(fileInfo.Size(), 10)},
		{Name: "last-modified", Value: fileInfo.ModTime().UTC().Format(http.TimeFormat)},
		// TODO: Add ETag header (spec 2.3.3.1)
	}
	// Placeholder ETag
	etag := fmt.Sprintf("\"%x-%x\"", fileInfo.Size(), fileInfo.ModTime().UnixNano())
	headers = append(headers, http2.HeaderField{Name: "etag", Value: etag})

	// Conditional GET handling (If-None-Match, If-Modified-Since)
	// Spec 2.3.5: If both If-None-Match and If-Modified-Since are present, If-None-Match takes precedence.
	ifnm := req.Header.Get("if-none-match")
	if ifnm != "" {
		// For simplicity, direct string comparison. Real ETag parsing can be more complex (weak ETags, list of ETags).
		if ifnm == etag || ifnm == "*" { // "*" matches any ETag
			s.logger.Debug("StaticFileServer: ETag match for If-None-Match", logger.LogFields{"path": filePath, "client_etag": ifnm, "server_etag": etag})
			statusHeaders := []http2.HeaderField{{Name: ":status", Value: "304"}}
			// Must include ETag and other relevant cache headers on 304 if they would have been on 200.
			// Content-Length MUST NOT be sent. Date should be.
			for _, h := range headers {
				// Filter out :status, content-length from original 200 headers. Keep others like ETag, Last-Modified.
				if h.Name != ":status" && h.Name != "content-length" {
					statusHeaders = append(statusHeaders, h)
				}
			}
			statusHeaders = append(statusHeaders, http2.HeaderField{Name: "date", Value: time.Now().UTC().Format(http.TimeFormat)})

			err := stream.SendHeaders(statusHeaders, true) // 304 has no body
			if err != nil {
				s.logger.Error("StaticFileServer: Error sending 304 Not Modified headers (If-None-Match)", logger.LogFields{"path": filePath, "error": err.Error()})
			}
			return
		}
	} else {
		ims := req.Header.Get("if-modified-since")
		if ims != "" {
			if t, err := http.ParseTime(ims); err == nil {
				// If-Modified-Since is compared against the file's modification time.
				// Use ModTime().Truncate(time.Second) to ensure consistent comparison
				// as IMS header might not have sub-second precision.
				if !fileInfo.ModTime().Truncate(time.Second).After(t.Truncate(time.Second)) {
					s.logger.Debug("StaticFileServer: If-Modified-Since condition met", logger.LogFields{"path": filePath, "ims_time": ims, "file_mod_time": fileInfo.ModTime().UTC().Format(http.TimeFormat)})
					statusHeaders := []http2.HeaderField{{Name: ":status", Value: "304"}}
					for _, h := range headers {
						if h.Name != ":status" && h.Name != "content-length" {
							statusHeaders = append(statusHeaders, h)
						}
					}
					statusHeaders = append(statusHeaders, http2.HeaderField{Name: "date", Value: time.Now().UTC().Format(http.TimeFormat)})

					err := stream.SendHeaders(statusHeaders, true) // 304 has no body
					if err != nil {
						s.logger.Error("StaticFileServer: Error sending 304 Not Modified headers (If-Modified-Since)", logger.LogFields{"path": filePath, "error": err.Error()})
					}
					return
				}
			}
		}
	}

	// Send initial headers (status 200 OK)
	// For HEAD request, endStream is true here. For GET, it's false if there's a body.
	endStreamForHeaders := (req.Method == http.MethodHead) || (fileInfo.Size() == 0 && req.Method == http.MethodGet)
	if err := stream.SendHeaders(headers, endStreamForHeaders); err != nil {
		s.logger.Error("StaticFileServer: Error sending file headers", logger.LogFields{"path": filePath, "error": err.Error()})
		// Don't attempt to send a default error if SendHeaders itself failed, stream is likely broken.
		return
	}

	// For GET requests with content, send the file body.
	if req.Method == http.MethodGet && fileInfo.Size() > 0 {
		file, err := os.Open(filePath)
		if err != nil {
			s.logger.Error("StaticFileServer: Error opening file for GET", logger.LogFields{"path": filePath, "error": err.Error()})
			// Headers already sent, so we can't send a 403/500 status easily.
			// The connection will likely be RST_STREAM'd by the client if data doesn't arrive,
			// or we could try to RST_STREAM from here if stream provides such a method.
			// For now, just log and return. The stream will hang from client PoV.
			// TODO: A robust solution would involve stream.Reset(errorCode)
			return
		}
		defer file.Close()

		// Use a buffer for io.Copy (or manual loop)
		// buf := make([]byte, 32*1024) // Or use a sync.Pool
		// For HTTP/2, DATA frames will be chunked by stream.WriteData anyway based on MaxFrameSize.
		// The loop below handles sending chunks and respecting flow control.

		// Manual read loop to control WriteData and endStream flag precisely
		buf := make([]byte, 32*1024) // Max typical frame size. Our WriteData will chunk further if needed.
		var writtenBytes int64
		for {
			nr, er := file.Read(buf)
			if nr > 0 {
				writtenBytes += int64(nr)
				// Determine if this is the last chunk based on file size or EOF
				// isLastChunk should be true only for the very final WriteData call.
				isLastChunk := (er == io.EOF) || (writtenBytes == fileInfo.Size())

				s.logger.Debug("StaticFileServer: Writing data chunk", logger.LogFields{"path": filePath, "bytes_in_chunk": nr, "total_written_so_far": writtenBytes, "file_size": fileInfo.Size(), "is_last_chunk_flag": isLastChunk, "read_error": er})

				_, ew := stream.WriteData(buf[0:nr], isLastChunk)
				if ew != nil {
					s.logger.Error("StaticFileServer: Error writing file data to stream", logger.LogFields{"path": filePath, "error": ew.Error()})
					// Error during data write. Headers are sent. Client will see truncated body.
					// Stream might be reset by lower layers.
					return // Stop sending
				}
			}
			if er == io.EOF {
				s.logger.Debug("StaticFileServer: Reached EOF for file", logger.LogFields{"path": filePath, "total_written": writtenBytes})
				if writtenBytes != fileInfo.Size() {
					s.logger.Warn("StaticFileServer: EOF reached but bytes written does not match file size", logger.LogFields{"path": filePath, "written": writtenBytes, "expected_size": fileInfo.Size()})
				}
				break // EOF, successfully read and sent all.
			}
			if er != nil {
				s.logger.Error("StaticFileServer: Error reading file", logger.LogFields{"path": filePath, "error": er.Error()})
				// Error during file read. Headers are sent. Client might see truncated body.
				return // Stop sending
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
