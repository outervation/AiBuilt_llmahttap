package staticfile

import (
	"fmt"
	"html"
	"io"
	"mime" // Added
	"net/http"
	"net/url" // Added
	"os"
	"path" // Added
	"path/filepath"
	"sort" // Added
	"strconv"
	"strings"

	"example.com/llmahttap/v2/internal/config"
	"example.com/llmahttap/v2/internal/handlers/staticfileserver" // for MimeTypeResolver
	"example.com/llmahttap/v2/internal/http2"                     // For http2.StreamWriter
	"example.com/llmahttap/v2/internal/logger"
	"example.com/llmahttap/v2/internal/router"
	"example.com/llmahttap/v2/internal/server" // For server.Handler

	"github.com/dustin/go-humanize" // Added
)

// StaticFileServer handles serving static files.
type StaticFileServer struct {
	Config       *config.StaticFileServerConfig
	logger       *logger.Logger
	MimeResolver *staticfileserver.MimeTypeResolver // Changed from mimeResolver to MimeResolver
}

// New creates a new StaticFileServer handler.
// It conforms to the server.HandlerFactory signature when partially applied or wrapped.
func New(cfg *config.StaticFileServerConfig, lg *logger.Logger, mainConfigFilePath string) (server.Handler, error) {
	if cfg == nil {
		return nil, fmt.Errorf("StaticFileServer: config cannot be nil")
	}
	if lg == nil {
		// Fallback to a discard logger if none is provided.
		// This can happen if the HandlerRegistry doesn't enforce a non-nil logger.
		// Or if a test doesn't provide one, though test helpers usually set a default.
		lg = logger.NewDiscardLogger()
		lg.Warn("StaticFileServer.New: Logger was nil, using discard logger. This might indicate an issue in handler instantiation.", nil)
	}

	// MimeTypeResolver requires StaticFileServerConfig, not json.RawMessage
	// The config passed to New is already resolved and validated.
	resolver, err := staticfileserver.NewMimeTypeResolver(cfg, mainConfigFilePath)
	if err != nil {
		lg.Error("StaticFileServer.New: Failed to create MimeTypeResolver", logger.LogFields{"error": err, "mainConfigPath": mainConfigFilePath})
		return nil, fmt.Errorf("StaticFileServer: failed to create MimeTypeResolver: %w", err)
	}

	return &StaticFileServer{
		Config:       cfg,
		logger:       lg,
		MimeResolver: resolver, // Corrected from mimeResolver
	}, nil
}

// ServeHTTP2 implements the server.Handler interface.
// This is a placeholder implementation.

func (s *StaticFileServer) ServeHTTP2(stream http2.StreamWriter, req *http.Request) {
	s.logger.Debug("StaticFileServer.ServeHTTP2: ENTERED", logger.LogFields{
		"method":      req.Method,
		"uri":         req.RequestURI,
		"stream_id":   stream.ID(),
		"remote_addr": req.RemoteAddr,
	})

	// Spec 2.3.2: Handle OPTIONS first.
	// This applies to the handler's general capability for paths matching the route,
	// not necessarily to an existing specific resource.
	if req.Method == http.MethodOptions {
		s.logger.Debug("StaticFileServer: Handling OPTIONS (top-level)", logger.LogFields{"stream_id": stream.ID(), "path": req.URL.Path})
		headers := []http2.HeaderField{
			{Name: ":status", Value: "204"}, // No Content
			{Name: "allow", Value: "GET, HEAD, OPTIONS"},
			{Name: "content-length", Value: "0"},
		}
		err := stream.SendHeaders(headers, true) // true to end stream
		if err != nil {
			s.logger.Error("StaticFileServer: Error sending top-level OPTIONS response headers", logger.LogFields{"stream_id": stream.ID(), "path": req.URL.Path, "error": err.Error()})
		}
		return
	}

	// Extract matched PathPattern from context (set by router)
	// This is crucial for correct sub-path resolution (Spec 2.3.1)
	matchedPathPatternVal := req.Context().Value(router.MatchedPathPatternKey{})
	matchedPathPattern, ok := matchedPathPatternVal.(string)
	if !ok {
		s.logger.Error("StaticFileServer: MatchedPathPatternKey not found or not a string in request context", logger.LogFields{"stream_id": stream.ID()})
		server.SendDefaultErrorResponse(stream, http.StatusInternalServerError, req, "Server configuration error (routing context).", s.logger)
		return
	}

	// Path resolution and security checks (Spec 2.3.1)
	if !strings.HasPrefix(req.URL.Path, matchedPathPattern) {
		s.logger.Error("StaticFileServer: Request path does not match the routed PathPattern prefix.", logger.LogFields{
			"stream_id":            stream.ID(),
			"request_path":         req.URL.Path,
			"matched_path_pattern": matchedPathPattern,
		})
		server.SendDefaultErrorResponse(stream, http.StatusInternalServerError, req, "Server routing inconsistency.", s.logger)
		return
	}

	subPath := strings.TrimPrefix(req.URL.Path, matchedPathPattern)

	// Call _resolvePath which handles canonicalization, security checks, and os.Stat
	targetPath, fileInfo, httpStatusCode, resolveErr, isTraversal := s._resolvePath(subPath, stream.ID(), req.URL.Path)
	if resolveErr != nil {
		s.logger.Warn("StaticFileServer: Path resolution error", logger.LogFields{
			"stream_id":    stream.ID(),
			"sub_path":     subPath,
			"doc_root":     s.Config.DocumentRoot,
			"resolved_err": resolveErr.Error(),
			"httpStatus":   httpStatusCode, // Log the status code from _resolvePath
			"isTraversal":  isTraversal,
		})

		// Use httpStatusCode returned by _resolvePath
		var clientMessage string
		switch httpStatusCode {
		case http.StatusNotFound:
			if isTraversal {
				clientMessage = "Resource not found (invalid path)."
			} else {
				clientMessage = "File not found."
			}
		case http.StatusForbidden:
			clientMessage = "Access denied."
		case http.StatusInternalServerError:
			clientMessage = "Error resolving file path."
		default: // Should not happen if _resolvePath is correct and returns a valid status on error
			s.logger.Error("StaticFileServer: _resolvePath returned unexpected status code or unhandled error state", logger.LogFields{
				"stream_id":      stream.ID(),
				"error":          resolveErr, // Log the original error
				"httpStatusCode": httpStatusCode,
			})
			httpStatusCode = http.StatusInternalServerError // Default to 500 if status code was 0 or unexpected
			clientMessage = "Internal server error during path resolution."
		}
		server.SendDefaultErrorResponse(stream, httpStatusCode, req, clientMessage, s.logger)
		return
	}

	s.logger.Debug("StaticFileServer: Path resolved successfully", logger.LogFields{
		"stream_id":  stream.ID(),
		"sub_path":   subPath,
		"targetPath": targetPath,
		"isDir":      fileInfo.IsDir(),
	})

	// fileInfo is populated by _resolvePath if no error occurred.
	// No need for another os.Stat call here.

	// Handle based on file or directory
	if fileInfo.IsDir() {
		s.handleDirectory(stream, req, targetPath, fileInfo, req.URL.Path) // Pass original web path for display
	} else {
		s.handleFile(stream, req, targetPath, fileInfo)
	}
}

func (s *StaticFileServer) handleDirectory(stream http2.StreamWriter, req *http.Request, dirPath string, dirInfo os.FileInfo, displayPath string) {
	s.logger.Debug("StaticFileServer: Handling directory", logger.LogFields{"stream_id": stream.ID(), "dirPath": dirPath, "displayPath": displayPath})

	// Spec 2.3.2: Supported HTTP Methods. For directories, this applies to index file serving or listing.
	// OPTIONS is handled at ServeHTTP2 level. GET/HEAD are primary.
	if req.Method != http.MethodGet && req.Method != http.MethodHead {
		s.logger.Warn("StaticFileServer: Method not allowed for directory", logger.LogFields{
			"stream_id": stream.ID(),
			"method":    req.Method,
			"dirPath":   dirPath,
		})
		// Drain request body before sending 405
		if req.Body != nil {
			_, _ = io.Copy(io.Discard, req.Body)
			req.Body.Close()
		}
		server.SendDefaultErrorResponse(stream, http.StatusMethodNotAllowed, req, "Method not allowed for this resource.", s.logger)
		return
	}

	// Spec 2.2.2: Attempt to serve an IndexFiles entry.
	indexFiles := s.Config.IndexFiles
	if len(indexFiles) == 0 && (s.Config.IndexFiles == nil || len(s.Config.IndexFiles) == 0) {
	}

	for _, indexName := range s.Config.IndexFiles {
		if indexName == "" {
			continue
		}
		indexPath := filepath.Join(dirPath, indexName)
		indexInfo, err := os.Stat(indexPath)
		if err == nil && !indexInfo.IsDir() {
			s.logger.Info("StaticFileServer: Serving index file from directory", logger.LogFields{
				"stream_id": stream.ID(),
				"dirPath":   dirPath,
				"indexFile": indexPath,
			})
			s.handleFile(stream, req, indexPath, indexInfo)
			return
		}
		if err != nil && !os.IsNotExist(err) {
			s.logger.Error("StaticFileServer: Error stating potential index file", logger.LogFields{
				"stream_id": stream.ID(),
				"indexPath": indexPath,
				"error":     err.Error(),
			})
		}
	}

	// Spec 2.2.3: No IndexFiles found. Check ServeDirectoryListing.
	serveListing := false
	if s.Config.ServeDirectoryListing != nil {
		serveListing = *s.Config.ServeDirectoryListing
	}

	if !serveListing {
		s.logger.Info("StaticFileServer: No index file and directory listing disabled, sending 403 Forbidden", logger.LogFields{
			"stream_id": stream.ID(),
			"dirPath":   dirPath,
		})
		server.SendDefaultErrorResponse(stream, http.StatusForbidden, req, "Access to this directory is forbidden.", s.logger)
		return
	}

	// Serve directory listing
	s.logger.Info("StaticFileServer: No index file found, serving directory listing", logger.LogFields{
		"stream_id": stream.ID(),
		"dirPath":   dirPath,
		"webPath":   displayPath,
	})

	entries, err := os.ReadDir(dirPath)
	if err != nil {
		s.logger.Error("StaticFileServer: Error reading directory for listing", logger.LogFields{
			"stream_id": stream.ID(),
			"dirPath":   dirPath,
			"error":     err.Error(),
		})
		server.SendDefaultErrorResponse(stream, http.StatusInternalServerError, req, "Error reading directory contents.", s.logger)
		return
	}

	htmlBytes, err := s.generateDirectoryListingHTML(displayPath, entries, dirPath)
	if err != nil {
		s.logger.Error("StaticFileServer: Error generating directory listing HTML", logger.LogFields{
			"stream_id": stream.ID(),
			"dirPath":   dirPath,
			"error":     err.Error(),
		})
		server.SendDefaultErrorResponse(stream, http.StatusInternalServerError, req, "Error generating directory listing.", s.logger)
		return
	}

	headers := []http2.HeaderField{
		{Name: ":status", Value: "200"},
		{Name: "content-type", Value: "text/html; charset=utf-8"},
		{Name: "content-length", Value: strconv.Itoa(len(htmlBytes))},
		{Name: "cache-control", Value: "no-cache, no-store, must-revalidate"},
		{Name: "pragma", Value: "no-cache"},
		{Name: "expires", Value: "0"},
	}

	isLastFrame := req.Method == http.MethodHead || (len(htmlBytes) == 0 && req.Method == http.MethodGet)
	err = stream.SendHeaders(headers, isLastFrame)
	if err != nil {
		s.logger.Error("StaticFileServer: Error sending headers for directory listing", logger.LogFields{"stream_id": stream.ID(), "dirPath": dirPath, "error": err.Error()})
		return
	}
	if req.Method == http.MethodGet && len(htmlBytes) > 0 {
		_, err = stream.WriteData(htmlBytes, true)
		if err != nil {
			s.logger.Error("StaticFileServer: Error writing directory listing data to stream", logger.LogFields{"stream_id": stream.ID(), "dirPath": dirPath, "error": err.Error()})
		}
	}
	s.logger.Debug("StaticFileServer: Finished handling directory", logger.LogFields{"stream_id": stream.ID(), "dirPath": dirPath})
}

func (s *StaticFileServer) handleFile(stream http2.StreamWriter, req *http.Request, filePath string, fileInfo os.FileInfo) {
	s.logger.Debug("StaticFileServer: Handling file", logger.LogFields{
		"stream_id": stream.ID(),
		"filePath":  filePath,
		"method":    req.Method,
		"size":      fileInfo.Size(),
	})

	// Spec 2.3.2: Supported HTTP Methods. Primarily GET and HEAD.
	// OPTIONS is handled at ServeHTTP2 level.
	if req.Method != http.MethodGet && req.Method != http.MethodHead {
		s.logger.Warn("StaticFileServer: Method not allowed for file", logger.LogFields{
			"stream_id": stream.ID(),
			"method":    req.Method,
			"filePath":  filePath,
		})
		// Drain request body before sending 405
		if req.Body != nil {
			io.Copy(io.Discard, req.Body)
			req.Body.Close()
		}
		server.SendDefaultErrorResponse(stream, http.StatusMethodNotAllowed, req, "Method not allowed for this resource.", s.logger)
		return
	}

	// Conditional request handling (Spec 2.3.5)
	etag := generateETag(fileInfo)
	lastModified := fileInfo.ModTime()

	// Check If-None-Match
	if inm := req.Header.Get("If-None-Match"); inm != "" {
		match := false
		clientEtags := strings.Split(inm, ",")
		for _, clientEtag := range clientEtags {
			clientEtag = strings.TrimSpace(clientEtag)
			isClientWeak := strings.HasPrefix(clientEtag, "W/")
			if isClientWeak {
				clientEtag = strings.TrimPrefix(clientEtag, "W/")
			}
			serverEtagTrimmed := strings.Trim(etag, "\"")
			clientEtagTrimmed := strings.Trim(clientEtag, "\"")

			if clientEtagTrimmed == serverEtagTrimmed || clientEtag == "*" {
				match = true
				break
			}
		}

		if match {
			s.logger.Info("StaticFileServer: If-None-Match cache hit", logger.LogFields{"stream_id": stream.ID(), "filePath": filePath, "etag": etag})
			headers := []http2.HeaderField{
				{Name: ":status", Value: "304"},
				{Name: "etag", Value: etag},
				{Name: "last-modified", Value: lastModified.UTC().Format(http.TimeFormat)},
			}
			stream.SendHeaders(headers, true)
			return
		}
	} else {
		if ims := req.Header.Get("If-Modified-Since"); ims != "" {
			if t, err := http.ParseTime(ims); err == nil {
				if !lastModified.After(t) {
					s.logger.Info("StaticFileServer: If-Modified-Since cache hit", logger.LogFields{"stream_id": stream.ID(), "filePath": filePath, "lastModified": lastModified, "ifModifiedSince": t})
					headers := []http2.HeaderField{
						{Name: ":status", Value: "304"},
						{Name: "etag", Value: etag},
						{Name: "last-modified", Value: lastModified.UTC().Format(http.TimeFormat)},
					}
					stream.SendHeaders(headers, true)
					return
				}
			} else {
				s.logger.Debug("StaticFileServer: Invalid If-Modified-Since date format", logger.LogFields{"stream_id": stream.ID(), "filePath": filePath, "ims_value": ims, "error": err.Error()})
			}
		}
	}

	var file *os.File
	var openErr error
	if req.Method == http.MethodGet {
		file, openErr = os.Open(filePath)
		if openErr != nil {
			if os.IsPermission(openErr) {
				s.logger.Warn("StaticFileServer: Permission denied opening file for GET", logger.LogFields{"stream_id": stream.ID(), "filePath": filePath, "error": openErr.Error()})
				server.SendDefaultErrorResponse(stream, http.StatusForbidden, req, "Access denied while opening file.", s.logger)
			} else {
				s.logger.Error("StaticFileServer: Error opening file for GET", logger.LogFields{"stream_id": stream.ID(), "filePath": filePath, "error": openErr.Error()})
				server.SendDefaultErrorResponse(stream, http.StatusInternalServerError, req, "Error opening file.", s.logger)
			}
			return
		}
		defer file.Close()
	}

	headers := []http2.HeaderField{
		{Name: ":status", Value: "200"},
		{Name: "content-type", Value: s.getMimeType(filePath)},
		{Name: "content-length", Value: strconv.FormatInt(fileInfo.Size(), 10)},
		{Name: "last-modified", Value: lastModified.UTC().Format(http.TimeFormat)},
		{Name: "etag", Value: etag},
	}

	endStreamWithHeaders := req.Method == http.MethodHead || (req.Method == http.MethodGet && fileInfo.Size() == 0)
	err := stream.SendHeaders(headers, endStreamWithHeaders)
	if err != nil {
		s.logger.Error("StaticFileServer: Error sending file response headers", logger.LogFields{"stream_id": stream.ID(), "filePath": filePath, "error": err.Error()})
		return
	}

	s.logger.Debug("StaticFileServer: Sent headers for file", logger.LogFields{
		"stream_id":            stream.ID(),
		"filePath":             filePath,
		"endStreamWithHeaders": endStreamWithHeaders,
		"num_headers":          len(headers),
	})

	if req.Method == http.MethodGet && fileInfo.Size() > 0 {
		buf := make([]byte, 32*1024)
		var writtenBytes int64
		for {
			nr, er := file.Read(buf)
			if nr > 0 {
				writtenBytes += int64(nr)
				isLastChunk := (er == io.EOF) || (writtenBytes == fileInfo.Size())

				s.logger.Debug("StaticFileServer: Writing data chunk", logger.LogFields{
					"stream_id": stream.ID(),
					"filePath":  filePath,
					"bytes":     nr,
					"isLast":    isLastChunk,
				})
				_, ew := stream.WriteData(buf[0:nr], isLastChunk)
				if ew != nil {
					s.logger.Error("StaticFileServer: Error writing file data to stream", logger.LogFields{"stream_id": stream.ID(), "filePath": filePath, "error": ew.Error()})
					return
				}
			}
			if er == io.EOF {
				s.logger.Debug("StaticFileServer: EOF reached for file", logger.LogFields{"stream_id": stream.ID(), "filePath": filePath, "totalWritten": writtenBytes})
				break
			}
			if er != nil {
				s.logger.Error("StaticFileServer: Error reading file content", logger.LogFields{"stream_id": stream.ID(), "filePath": filePath, "error": er.Error()})
				return
			}
		}
		s.logger.Info("StaticFileServer: Successfully served file", logger.LogFields{"stream_id": stream.ID(), "filePath": filePath, "bytes_sent": writtenBytes})
	} else if req.Method == http.MethodGet && fileInfo.Size() == 0 {
		s.logger.Info("StaticFileServer: Successfully served 0-byte file (headers only)", logger.LogFields{"stream_id": stream.ID(), "filePath": filePath})
	} else if req.Method == http.MethodHead {
		s.logger.Info("StaticFileServer: Successfully served HEAD request (headers only)", logger.LogFields{"stream_id": stream.ID(), "filePath": filePath})
	}
}

func (s *StaticFileServer) getMimeType(filePath string) string {
	if s.MimeResolver == nil { // Corrected from s.mimeResolver
		// This case should ideally not happen if New ensures MimeResolver is initialized.
		// Fallback to Go's default and then application/octet-stream.
		s.logger.Warn("StaticFileServer.getMimeType: MimeResolver is nil. Falling back to basic MIME detection.", logger.LogFields{"filePath": filePath})
		mimeType := mime.TypeByExtension(filepath.Ext(filePath))
		if mimeType == "" {
			return "application/octet-stream"
		}
		// Add charset=utf-8 for text types by default, if not already specified by TypeByExtension.
		if strings.HasPrefix(mimeType, "text/") && !strings.Contains(mimeType, "charset") {
			mimeType += "; charset=utf-8"
		}
		return mimeType
	}
	return s.MimeResolver.GetMimeType(filePath) // Corrected from s.mimeResolver
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

// _resolvePath handles path construction, canonicalization, security checks, and stat-ing.
// It returns the canonical path, file info, an HTTP status code for errors (404, 403, 500),
// the underlying error, and a boolean indicating if it was a path traversal attempt.
func (s *StaticFileServer) _resolvePath(subPath string, streamID uint32, reqURLPathForLog string) (
	resolvedPath string, fileInfo os.FileInfo, httpStatusCode int, err error, isTraversalAttempt bool,
) {
	// Append subPath to DocumentRoot
	// DocumentRoot is guaranteed to be absolute by config validation.
	targetPath := filepath.Join(s.Config.DocumentRoot, subPath)

	// Canonicalize path (e.g., resolving ., ..)
	canonicalPath, absErr := filepath.Abs(targetPath)
	if absErr != nil {
		s.logger.Error("StaticFileServer: Failed to canonicalize path", logger.LogFields{
			"stream_id":   streamID,
			"target_path": targetPath,
			"error":       absErr.Error(),
		})
		// For path canonicalization errors, treat as internal server error, not specifically traversal.
		return "", nil, http.StatusInternalServerError, fmt.Errorf("error processing file path: %w", absErr), false
	}

	// Security check: Ensure canonicalized path is still within DocumentRoot (Spec 2.3.1)
	// Clean the documentRoot path to remove any trailing slashes for a consistent prefix check, unless it's the root "/"
	cleanedDocumentRoot := filepath.Clean(s.Config.DocumentRoot)
	// If DocumentRoot was originally "/", filepath.Clean("/") is "/".
	// If DocumentRoot was "/foo/", filepath.Clean("/foo/") is "/foo".

	isWithinRoot := false
	if cleanedDocumentRoot == "/" {
		isWithinRoot = strings.HasPrefix(canonicalPath, cleanedDocumentRoot)
	} else {
		isWithinRoot = canonicalPath == cleanedDocumentRoot || strings.HasPrefix(canonicalPath, cleanedDocumentRoot+string(filepath.Separator))
	}

	if !isWithinRoot {
		s.logger.Warn("StaticFileServer: Attempt to access path outside document root (Path Traversal)", logger.LogFields{
			"stream_id":      streamID,
			"requested_path": reqURLPathForLog,
			"target_path":    targetPath,
			"canonical_path": canonicalPath,
			"document_root":  s.Config.DocumentRoot,
		})
		// Spec: 404 for path traversal (to avoid leaking info)
		return "", nil, http.StatusNotFound, fmt.Errorf("path is outside document root: %s", canonicalPath), true
	}

	// Check file existence and type
	fi, statErr := os.Stat(canonicalPath)
	if statErr != nil {
		if os.IsNotExist(statErr) {
			s.logger.Info("StaticFileServer: File or directory not found by _resolvePath", logger.LogFields{
				"stream_id": streamID,
				"path":      canonicalPath,
			})
			return "", nil, http.StatusNotFound, fmt.Errorf("file or directory not found: %w", statErr), false
		} else if os.IsPermission(statErr) {
			s.logger.Warn("StaticFileServer: Permission denied accessing path by _resolvePath", logger.LogFields{
				"stream_id": streamID,
				"path":      canonicalPath,
				"error":     statErr.Error(),
			})
			return "", nil, http.StatusForbidden, fmt.Errorf("permission denied for path: %w", statErr), false
		} else {
			s.logger.Error("StaticFileServer: Error stating file in _resolvePath", logger.LogFields{
				"stream_id": streamID,
				"path":      canonicalPath,
				"error":     statErr.Error(),
			})
			return "", nil, http.StatusInternalServerError, fmt.Errorf("error stating file: %w", statErr), false
		}
	}
	return canonicalPath, fi, 0, nil, false // 0 indicates success (no HTTP error code from this stage)
}

// generateETag creates a strong ETag for a file based on its size and modification time.
// Format: "<size_hex>-<modtime_unixnano_hex>"

// generateDirectoryListingHTML creates an HTML page listing the contents of a directory.
// webPath is the URL path that corresponds to this directory, used for link generation.
// entries are the os.DirEntry items for the directory.
// dirPath is the absolute filesystem path (used for logging and potentially re-stating if needed, though entries should be sufficient)

func (s *StaticFileServer) generateDirectoryListingHTML(webPath string, entries []os.DirEntry, dirPath string) ([]byte, error) {
	s.logger.Debug("Generating directory listing", logger.LogFields{"dirPath": dirPath, "webPath": webPath, "num_entries": len(entries)})

	sort.Slice(entries, func(i, j int) bool {
		infoI, errI := entries[i].Info()
		infoJ, errJ := entries[j].Info()

		isDirI := false
		if errI == nil {
			isDirI = infoI.IsDir()
		} else {
			s.logger.Warn("Error getting FileInfo for sorting directory entry", logger.LogFields{"entry": entries[i].Name(), "dirPath": dirPath, "error": errI.Error()})
		}

		isDirJ := false
		if errJ == nil {
			isDirJ = infoJ.IsDir()
		} else {
			s.logger.Warn("Error getting FileInfo for sorting directory entry", logger.LogFields{"entry": entries[j].Name(), "dirPath": dirPath, "error": errJ.Error()})
		}

		if isDirI != isDirJ {
			return isDirI
		}
		return strings.ToLower(entries[i].Name()) < strings.ToLower(entries[j].Name())
	})

	var sb strings.Builder
	escapedWebPath := html.EscapeString(webPath)
	sb.WriteString(fmt.Sprintf("<html><head><title>Index of %s</title></head><body>\n", escapedWebPath))
	sb.WriteString(fmt.Sprintf("<h1>Index of %s</h1><hr><pre>\n", escapedWebPath))

	if webPath != "/" {
		parentWebPath := path.Join(webPath, "..")
		if !strings.HasSuffix(parentWebPath, "/") && parentWebPath != "/" {
			parentWebPath += "/"
		}
		// Parent href should also be escaped if it could contain special chars, though ".." is safe.
		// For consistency, escape it. Path.Join cleans it first.
		sb.WriteString(fmt.Sprintf("<a href=\"%s\">../</a>\n", html.EscapeString(parentWebPath)))
	}

	for _, entry := range entries {
		entryName := entry.Name()

		fileInfo, err := entry.Info()
		if err != nil {
			s.logger.Warn("Could not get info for directory entry during listing generation, skipping", logger.LogFields{"entry": entryName, "dirPath": dirPath, "error": err.Error()})
			sb.WriteString(fmt.Sprintf("%s - Error getting info\n", html.EscapeString(entryName)))
			continue
		}

		// URL Encode the entry name for the href path component
		escapedEntryNameForURL := url.PathEscape(entryName)

		hrefSuffix := escapedEntryNameForURL
		if fileInfo.IsDir() {
			hrefSuffix += "/"
		}

		linkHref := ""
		if webPath == "/" {
			linkHref = "/" + hrefSuffix
		} else {
			// Ensure webPath (the base) does not have its existing slashes encoded by mistake
			// by only encoding the new component `hrefSuffix`.
			linkHref = strings.TrimSuffix(webPath, "/") + "/" + hrefSuffix
		}
		// The full linkHref should not be html.EscapeString'd if parts are already url.PathEscape'd.
		// The browser expects raw, percent-encoded URLs in href.

		displayName := html.EscapeString(entryName) // Display name is HTML escaped
		if fileInfo.IsDir() {
			displayName += "/"
		}

		sb.WriteString(fmt.Sprintf("<a href=\"%s\">%s</a>", linkHref, displayName))

		namePad := 50 - len(displayName)
		if namePad < 1 {
			namePad = 1
		}

		modTimeStr := fileInfo.ModTime().Format("02-Jan-2006 15:04")
		var sizeStr string
		if fileInfo.IsDir() {
			sizeStr = "-"
		} else {
			sizeStr = humanize.Bytes(uint64(fileInfo.Size()))
		}

		sb.WriteString(fmt.Sprintf("%*s %20s %10s\n",
			namePad, "",
			modTimeStr,
			sizeStr))
	}

	sb.WriteString("</pre><hr></body></html>\n")
	return []byte(sb.String()), nil
}

// This is copied from internal/handlers/staticfileserver/static_file_server.go
func generateETag(fi os.FileInfo) string {
	return fmt.Sprintf("\"%x-%x\"", fi.Size(), fi.ModTime().UnixNano())
}
