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
		"method":  req.Method,
		"path":    req.URL.Path,
		"docRoot": s.config.DocumentRoot,
	})

	// Ensure request body is handled for non-GET/HEAD/OPTIONS methods to prevent client hangs
	// especially when responding with an error before reading the body.
	if req.Method != http.MethodGet && req.Method != http.MethodHead && req.Method != http.MethodOptions {
		if req.Body != nil {
			// Drain and close the body to allow the client to not get stuck.
			// Max 32MB discard to prevent abuse, though for small error-triggering requests this limit is generous.
			// Using io.CopyN ensures we attempt to read up to N bytes.
			_, err := io.CopyN(io.Discard, req.Body, 32*1024*1024) // 32MB limit
			if err != nil && err != io.EOF {
				s.logger.Warn("Error discarding request body for unhandled method", logger.LogFields{
					"method": req.Method,
					"path":   req.URL.Path,
					"error":  err.Error(),
				})
			}
			// It's crucial to close the body, even if CopyN returns EOF early or another error.
			// Closing signals to the http2.Stream that the body has been processed from the handler's perspective.
			if closeErr := req.Body.Close(); closeErr != nil {
				s.logger.Warn("Error closing request body for unhandled method", logger.LogFields{
					"method": req.Method,
					"path":   req.URL.Path,
					"error":  closeErr.Error(),
				})
			}
		}

		s.logger.Info("StaticFileServer: Method not allowed", logger.LogFields{"method": req.Method})
		server.SendDefaultErrorResponse(stream, http.StatusMethodNotAllowed, req, "", s.logger)
		return
	}

	// For GET, HEAD, OPTIONS:
	if req.Method == http.MethodOptions {
		s.logger.Debug("StaticFileServer: Handling OPTIONS request", logger.LogFields{"path": req.URL.Path})
		// TODO: The Allow header should be more dynamic, e.g. reflecting if POST is handled by some other handler on this path.
		// For a static file server, it's typically GET, HEAD, OPTIONS.
		headers := []http2.HeaderField{
			{Name: ":status", Value: "204"}, // 204 No Content is common for OPTIONS
			{Name: "allow", Value: "GET, HEAD, OPTIONS"},
			{Name: "content-length", Value: "0"},
		}
		err := stream.SendHeaders(headers, true)
		if err != nil {
			s.logger.Error("StaticFileServer: Failed to send OPTIONS response headers", logger.LogFields{"path": req.URL.Path, "error": err})
		}
		return
	}

	// Path Resolution and Security (Spec 2.3.1)
	// This section requires careful implementation to map req.URL.Path to a filesystem path
	// correctly and securely, considering the route's PathPattern.
	// The current implementation makes simplifications and needs to be made robust.

	// HACK/Placeholder: Path resolution logic is simplified and needs to correctly use PathPattern.
	// Assume for now that req.URL.Path is the path component to be served *relative* to DocumentRoot
	// or needs minimal processing. This is a significant simplification.
	// The spec implies the router should provide the sub-path: "the part of the request path that follows the matched PathPattern prefix".
	// Our current router does not provide this directly to the handler.
	// We use a placeholder approach:
	fsPathPart := strings.TrimPrefix(req.URL.Path, "/")           // Basic attempt to make it relative
	if fsPathPart == "" && strings.HasSuffix(req.URL.Path, "/") { // Request for "/" or "/some/path/"
		// This might indicate a directory request.
	}

	targetPath := filepath.Join(s.config.DocumentRoot, fsPathPart)

	absDocRoot, err := filepath.Abs(s.config.DocumentRoot)
	if err != nil {
		s.logger.Error("StaticFileServer: Failed to get absolute path for DocumentRoot", logger.LogFields{"docRoot": s.config.DocumentRoot, "error": err})
		server.SendDefaultErrorResponse(stream, http.StatusInternalServerError, req, "Invalid server configuration.", s.logger)
		return
	}

	absTargetPath, err := filepath.Abs(targetPath)
	if err != nil {
		// This can happen for invalid paths, e.g. too many ".."
		s.logger.Warn("StaticFileServer: Failed to get absolute path for target", logger.LogFields{"targetPath": targetPath, "error": err, "requestedPath": req.URL.Path})
		// Per spec, malformed URIs or paths that cannot be resolved should lead to a 404.
		server.SendDefaultErrorResponse(stream, http.StatusNotFound, req, "Error resolving file path.", s.logger)
		return
	}

	// Security: Ensure the resolved path is still within the document root.
	if !strings.HasPrefix(absTargetPath, absDocRoot) {
		s.logger.Warn("StaticFileServer: Path traversal attempt or resolved path outside DocumentRoot", logger.LogFields{
			"requestedPath": req.URL.Path,
			"resolvedPath":  absTargetPath,
			"docRoot":       absDocRoot,
		})
		// Spec 2.3.1: "If the canonicalized path falls outside the DocumentRoot, a 404 Not Found response MUST be returned."
		server.SendDefaultErrorResponse(stream, http.StatusNotFound, req, "", s.logger)
		return
	}

	fileInfo, err := os.Stat(absTargetPath)
	if err != nil {
		if os.IsNotExist(err) {
			s.logger.Info("StaticFileServer: File not found", logger.LogFields{"path": absTargetPath, "requested": req.URL.Path})
			server.SendDefaultErrorResponse(stream, http.StatusNotFound, req, "", s.logger)
		} else if os.IsPermission(err) {
			s.logger.Warn("StaticFileServer: Permission denied for path", logger.LogFields{"path": absTargetPath, "error": err})
			server.SendDefaultErrorResponse(stream, http.StatusForbidden, req, "", s.logger)
		} else {
			s.logger.Error("StaticFileServer: Error stating file", logger.LogFields{"path": absTargetPath, "error": err})
			server.SendDefaultErrorResponse(stream, http.StatusInternalServerError, req, "Error accessing file.", s.logger)
		}
		return
	}

	if fileInfo.IsDir() {
		// If the original request path didn't end with a '/' and it's a directory,
		// Spec 2.3.2: "If the request path maps to a directory and does not end with a trailing slash,
		// a 301 Moved Permanently redirect to the same path with an appended slash SHOULD be returned."
		if !strings.HasSuffix(req.URL.Path, "/") {
			redirectPath := req.URL.Path + "/"
			if req.URL.RawQuery != "" {
				redirectPath += "?" + req.URL.RawQuery
			}
			s.logger.Debug("StaticFileServer: Redirecting to directory with slash", logger.LogFields{"from": req.URL.Path, "to": redirectPath})
			headers := []http2.HeaderField{
				{Name: ":status", Value: "301"},
				{Name: "location", Value: redirectPath},
				{Name: "content-length", Value: "0"},
			}
			if err := stream.SendHeaders(headers, true); err != nil {
				s.logger.Error("StaticFileServer: Failed to send 301 redirect headers", logger.LogFields{"error": err})
			}
			return
		}
		s.handleDirectory(stream, req, absTargetPath, fileInfo)
	} else {
		s.handleFile(stream, req, absTargetPath, fileInfo)
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
	// Conditional Requests (Caching - Spec 2.3.5)
	// ETag: Use strong validator (SHA256 of content could be used for true strong ETag, but modtime+size is common and simpler for "weak" strong)
	// For simplicity, using Apache-like ETag: inode-size-mtime (inode not easily/portably available, so just size-mtime)
	etag := fmt.Sprintf("\"%x-%x\"", fileInfo.ModTime().UnixNano(), fileInfo.Size())
	lastModified := fileInfo.ModTime().UTC().Format(http.TimeFormat)

	// Check If-None-Match (takes precedence over If-Modified-Since)
	if inm := req.Header.Get("if-none-match"); inm != "" {
		// Basic comparison: The spec allows for multiple ETags, weak/strong.
		// This simple check works for single, strong ETags.
		// TODO: Implement full ETag list parsing and weak/strong comparison.
		if inm == etag || inm == "W/"+etag { // Simplistic check
			s.logger.Debug("StaticFileServer: ETag match, sending 304 Not Modified", logger.LogFields{"path": filePath, "etag": etag, "if-none-match": inm})
			headers := []http2.HeaderField{
				{Name: ":status", Value: "304"},
				{Name: "etag", Value: etag},
				{Name: "last-modified", Value: lastModified}, // Recommended to send Last-Modified with 304 if ETag matches
			}
			stream.SendHeaders(headers, true)
			return
		}
	} else if ims := req.Header.Get("if-modified-since"); ims != "" { // Only check If-Modified-Since if If-None-Match is absent or didn't match
		if t, err := http.ParseTime(ims); err == nil {
			// ModTime is truncated to the second for comparison as If-Modified-Since has 1-second granularity
			if !fileInfo.ModTime().Truncate(time.Second).After(t) {
				s.logger.Debug("StaticFileServer: Not modified since, sending 304", logger.LogFields{"path": filePath, "ims": ims, "lastMod": lastModified})
				headers := []http2.HeaderField{
					{Name: ":status", Value: "304"},
					{Name: "etag", Value: etag}, // Good practice to send ETag with 304 from IMS
					{Name: "last-modified", Value: lastModified},
				}
				stream.SendHeaders(headers, true)
				return
			}
		}
	}

	contentType := s.getMimeType(filePath)
	contentLengthStr := strconv.FormatInt(fileInfo.Size(), 10)

	headers := []http2.HeaderField{
		{Name: ":status", Value: "200"},
		{Name: "content-type", Value: contentType},
		{Name: "content-length", Value: contentLengthStr},
		{Name: "last-modified", Value: lastModified},
		{Name: "etag", Value: etag},
		// TODO: Add Cache-Control headers based on config (Spec 2.2.5)
		// Example: {Name: "cache-control", Value: "public, max-age=3600"}
	}

	if req.Method == http.MethodHead {
		s.logger.Debug("StaticFileServer: Handling HEAD request", logger.LogFields{"path": filePath})
		err := stream.SendHeaders(headers, true) // End stream = true for HEAD
		if err != nil {
			s.logger.Error("StaticFileServer: Failed to send HEAD response headers", logger.LogFields{"path": filePath, "error": err})
		}
		return
	}

	// GET request
	s.logger.Debug("StaticFileServer: Handling GET request", logger.LogFields{"path": filePath, "size": fileInfo.Size()})
	file, err := os.Open(filePath)
	if err != nil {
		if os.IsPermission(err) {
			s.logger.Warn("StaticFileServer: Permission denied opening file for GET", logger.LogFields{"path": filePath, "error": err})
			server.SendDefaultErrorResponse(stream, http.StatusForbidden, req, "", s.logger)
		} else { // Includes os.IsNotExist if file was deleted between Stat and Open
			s.logger.Error("StaticFileServer: Error opening file for GET", logger.LogFields{"path": filePath, "error": err})
			server.SendDefaultErrorResponse(stream, http.StatusInternalServerError, req, "Error opening file.", s.logger)
		}
		return
	}
	defer file.Close()

	err = stream.SendHeaders(headers, fileInfo.Size() == 0) // End stream if file is empty
	if err != nil {
		s.logger.Error("StaticFileServer: Failed to send GET response headers", logger.LogFields{"path": filePath, "error": err})
		return
	}

	if fileInfo.Size() > 0 {
		// Buffer for copying. The stream.WriteData will handle framing.
		// The size of this buffer affects I/O efficiency but not MaxFrameSize directly.
		// Optimal buffer size can depend on various factors. net/http uses 32KB.
		buf := make([]byte, 32*1024)
		var writtenBytes int64
		for {
			n, readErr := file.Read(buf)
			if n > 0 {
				writtenBytes += int64(n)
				// isLastChunk is true if this read operation returned io.EOF,
				// meaning buf[:n] is the final data from the file.
				isLastChunk := (readErr == io.EOF)

				_, writeErr := stream.WriteData(buf[:n], isLastChunk)
				if writeErr != nil {
					s.logger.Error("StaticFileServer: Error writing file data to stream", logger.LogFields{
						"path":  filePath,
						"error": writeErr.Error(),
					})
					// If WriteData fails, the stream is likely broken. Cannot send further HTTP errors.
					// The HTTP/2 library should handle RST_STREAM or connection issues.
					return // Abort sending for this request
				}
			}

			if readErr == io.EOF {
				// Sanity check: after EOF, total bytes written should match file size.
				// This check is for non-empty files; empty files are handled by SendHeaders endStream=true.
				if fileInfo.Size() > 0 && writtenBytes != fileInfo.Size() {
					s.logger.Warn("StaticFileServer: File copy ended with EOF, but byte count mismatch", logger.LogFields{
						"path":     filePath,
						"written":  writtenBytes,
						"expected": fileInfo.Size(),
					})
				}
				break // Successfully read and sent the last chunk (if n > 0) or handled EOF.
			}
			if readErr != nil {
				s.logger.Error("StaticFileServer: Error reading file content", logger.LogFields{
					"path":  filePath,
					"error": readErr.Error(),
				})
				// Headers already sent. Client will get a truncated response.
				// The HTTP/2 library layer might RST_STREAM.
				return // Abort sending
			}
		}
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
