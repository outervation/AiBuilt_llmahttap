package staticfileserver

import (
	"encoding/json"
	"fmt"
	"mime"
	"os"
	"path/filepath"
	"strings"

	"example.com/llmahttap/v2/internal/config" // For StaticFileServerConfig.ResolvedMimeTypes and potentially path resolution
)

// defaultMimeTypes provides a basic set of common MIME types.
// More comprehensive lists can be found in Go's mime package or Apache's mime.types.
// This list is used as a fallback if Go's mime.TypeByExtension doesn't find a type
// and no custom types are provided.
// Spec 2.2.4: "server MUST use a comprehensive built-in set of common MIME types (e.g., derived from Go's mime.TypeByExtension)"
// We will primarily rely on mime.TypeByExtension and this map is a supplement/custom override mechanism.
var defaultMimeTypes = map[string]string{
	".aac":    "audio/aac",
	".abw":    "application/x-abiword",
	".apng":   "image/apng",
	".arc":    "application/x-freearc",
	".avif":   "image/avif",
	".avi":    "video/x-msvideo",
	".azw":    "application/vnd.amazon.ebook",
	".bin":    "application/octet-stream",
	".bmp":    "image/bmp",
	".bz":     "application/x-bzip",
	".bz2":    "application/x-bzip2",
	".cda":    "application/x-cdf",
	".csh":    "application/x-csh",
	".css":    "text/css; charset=utf-8",
	".csv":    "text/csv; charset=utf-8",
	".doc":    "application/msword",
	".docx":   "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
	".eot":    "application/vnd.ms-fontobject",
	".epub":   "application/epub+zip",
	".gz":     "application/gzip",
	".gif":    "image/gif",
	".htm":    "text/html; charset=utf-8",
	".html":   "text/html; charset=utf-8",
	".ico":    "image/vnd.microsoft.icon",
	".ics":    "text/calendar; charset=utf-8",
	".jar":    "application/java-archive",
	".jpeg":   "image/jpeg",
	".jpg":    "image/jpeg",
	".js":     "text/javascript; charset=utf-8",
	".json":   "application/json; charset=utf-8",
	".jsonld": "application/ld+json; charset=utf-8",
	".mid":    "audio/midi",
	".midi":   "audio/midi",
	".mjs":    "text/javascript; charset=utf-8",
	".mp3":    "audio/mpeg",
	".mp4":    "video/mp4",
	".mpeg":   "video/mpeg",
	".mpkg":   "application/vnd.apple.installer+xml",
	".odp":    "application/vnd.oasis.opendocument.presentation",
	".ods":    "application/vnd.oasis.opendocument.spreadsheet",
	".odt":    "application/vnd.oasis.opendocument.text",
	".oga":    "audio/ogg",
	".ogv":    "video/ogg",
	".ogx":    "application/ogg",
	".opus":   "audio/opus",
	".otf":    "font/otf",
	".png":    "image/png",
	".pdf":    "application/pdf",
	".php":    "application/x-httpd-php",
	".ppt":    "application/vnd.ms-powerpoint",
	".pptx":   "application/vnd.openxmlformats-officedocument.presentationml.presentation",
	".rar":    "application/vnd.rar",
	".rtf":    "application/rtf",
	".sh":     "application/x-sh",
	".svg":    "image/svg+xml",
	".tar":    "application/x-tar",
	".tif":    "image/tiff",
	".tiff":   "image/tiff",
	".ts":     "video/mp2t",
	".ttf":    "font/ttf",
	".txt":    "text/plain; charset=utf-8",
	".vsd":    "application/vnd.visio",
	".wav":    "audio/wav",
	".weba":   "audio/webm",
	".webm":   "video/webm",
	".webp":   "image/webp",
	".woff":   "font/woff",
	".woff2":  "font/woff2",
	".xhtml":  "application/xhtml+xml; charset=utf-8",
	".xls":    "application/vnd.ms-excel",
	".xlsx":   "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
	".xml":    "application/xml; charset=utf-8", // Note: text/xml also common, but application/xml is more standard.
	".xul":    "application/vnd.mozilla.xul+xml",
	".zip":    "application/zip",
	".3gp":    "video/3gpp",  // audio/3gpp if it doesn't contain video
	".3g2":    "video/3gpp2", // audio/3gpp2 if it doesn't contain video
	".7z":     "application/x-7z-compressed",
}

const defaultOctetStreamMimeType = "application/octet-stream"

// MimeTypeResolver encapsulates the logic for determining MIME types.
type MimeTypeResolver struct {
	customMimeTypes map[string]string // Resolved from config (inline map + file)
}

// NewMimeTypeResolver creates a MimeTypeResolver.
// It loads custom MIME types from the provided path (if any) and merges them with the inline map.
// The mainConfigFilePath is used to resolve relative paths for sfsConfig.MimeTypesPath.
func NewMimeTypeResolver(sfsConfig *config.StaticFileServerConfig, mainConfigFilePath string) (*MimeTypeResolver, error) {
	resolver := &MimeTypeResolver{
		customMimeTypes: make(map[string]string),
	}

	// 1. Load from inline map first (lower precedence)
	if sfsConfig.MimeTypesMap != nil {
		for ext, mimeType := range sfsConfig.MimeTypesMap {
			resolver.customMimeTypes[strings.ToLower(ext)] = mimeType
		}
	}

	// 2. Load from file (higher precedence, overrides inline)
	if sfsConfig.MimeTypesPath != nil && *sfsConfig.MimeTypesPath != "" {
		mimePath := *sfsConfig.MimeTypesPath
		if !filepath.IsAbs(mimePath) && mainConfigFilePath != "" {
			mimePath = filepath.Join(filepath.Dir(mainConfigFilePath), mimePath)
		}

		data, err := os.ReadFile(mimePath)
		if err != nil {
			return nil, &config.ConfigError{
				FilePath: mimePath,
				Message:  "failed to read custom MIME types file",
				Err:      err,
			}
		}

		var fileMimeTypes map[string]string
		if err := json.Unmarshal(data, &fileMimeTypes); err != nil {
			return nil, &config.ConfigError{
				FilePath: mimePath,
				Message:  "failed to parse custom MIME types JSON file",
				Err:      err,
			}
		}
		for ext, mimeType := range fileMimeTypes {
			resolver.customMimeTypes[strings.ToLower(ext)] = mimeType
		}
	}

	// Store the resolved types back in the config for potential reference or logging, though StaticFileServer will use the resolver.
	// This field `ResolvedMimeTypes` in `StaticFileServerConfig` is marked as `json:"-" toml:"-"`,
	// so it's for internal use post-config-parsing.
	if sfsConfig != nil {
		sfsConfig.ResolvedMimeTypes = resolver.customMimeTypes
	}

	return resolver, nil
}

// GetMimeType determines the MIME type for a given file path.
// It follows the logic specified in 2.2.4:
// 1. Check custom mappings (from config file or inline).
// 2. If not found, use Go's mime.TypeByExtension.
// 3. If still not found, use a built-in comprehensive set (our defaultMimeTypes map).
// 4. If still not found, default to application/octet-stream.
func (r *MimeTypeResolver) GetMimeType(filePath string) string {
	ext := strings.ToLower(filepath.Ext(filePath))

	if ext == "" { // No extension, cannot determine easily
		return defaultOctetStreamMimeType
	}

	// 1. Check custom mappings
	if mimeType, ok := r.customMimeTypes[ext]; ok {
		return mimeType
	}

	// 2. Use Go's mime.TypeByExtension
	// This itself includes a fairly comprehensive set of common types.
	if mimeType := mime.TypeByExtension(ext); mimeType != "" {
		// mime.TypeByExtension might return "type/subtype;param=value"
		// We typically only want "type/subtype" for Content-Type header unless charset is specifically handled.
		// For simplicity, let's assume it's usually fine, but if it includes charset, we need to be careful.
		// The spec for StaticFileServer (2.3.3.1) implies Content-Type is just the type.
		// text/* types usually benefit from charset=utf-8. Go's mime often adds this.
		return mimeType
	}

	// 3. Check our built-in defaultMimeTypes (as a further fallback or for types Go's stdlib might miss)
	if mimeType, ok := defaultMimeTypes[ext]; ok {
		return mimeType
	}

	// 4. Default to application/octet-stream
	return defaultOctetStreamMimeType
}

// LoadCustomMimeTypesFromFile reads a JSON file from filePath, parses it
// as a map of file extensions to MIME types, validates the entries,
// and returns the map.
// Keys (extensions) in the returned map are lowercased.
// As per spec 2.2.4, file extensions in the JSON file MUST start with a '.',
// and MIME type values MUST NOT be empty.
func LoadCustomMimeTypesFromFile(filePath string) (map[string]string, error) {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return nil, fmt.Errorf("failed to read MIME types file %q: %w", filePath, err)
	}

	// json.Unmarshal will error on empty data if it's not valid JSON (e.g. "{}")
	// An empty file is not valid JSON. A file containing "{}" is.
	if len(data) == 0 {
		// Consider returning an empty map or an error for a completely empty file.
		// For now, let json.Unmarshal handle this; it will error if data is empty.
		// If the file must represent a valid JSON object, then an empty file is an error.
		// If it can represent "no custom types", an empty map from "{}" is fine.
		// The current behavior is to error if not valid JSON.
	}

	var parsedMimeTypes map[string]string
	if err := json.Unmarshal(data, &parsedMimeTypes); err != nil {
		return nil, fmt.Errorf("failed to parse JSON from MIME types file %q: %w", filePath, err)
	}

	customMimeTypes := make(map[string]string)
	for ext, mimeType := range parsedMimeTypes {
		if !strings.HasPrefix(ext, ".") {
			return nil, fmt.Errorf("invalid extension %q in MIME types file %q: must start with a '.'", ext, filePath)
		}
		if mimeType == "" {
			return nil, fmt.Errorf("empty MIME type for extension %q in MIME types file %q", ext, filePath)
		}
		customMimeTypes[strings.ToLower(ext)] = mimeType
	}

	return customMimeTypes, nil
}

// ResolveMimeType determines the MIME type for a given file extension.
// It follows a specific order of precedence:
// 1. Checks the provided customUserMappings.
// 2. Checks the package-level defaultMimeTypes map.
// 3. Uses Go's mime.TypeByExtension.
// 4. Defaults to application/octet-stream if no type is found.
// The 'extension' argument should be the file extension including the leading dot (e.g., ".html").
func ResolveMimeType(extension string, customUserMappings map[string]string) string {
	if extension == "" {
		return defaultOctetStreamMimeType
	}

	// Ensure extension is consistently cased (lowercase) for map lookups
	ext := strings.ToLower(extension)

	// 1. Check customUserMappings
	if customUserMappings != nil {
		if mimeType, ok := customUserMappings[ext]; ok {
			return mimeType
		}
	}

	// 2. Check package-level defaultMimeTypes map
	if mimeType, ok := defaultMimeTypes[ext]; ok {
		return mimeType
	}

	// 3. Use Go's mime.TypeByExtension
	// This itself includes a fairly comprehensive set of common types.
	if mimeType := mime.TypeByExtension(ext); mimeType != "" {
		// mime.TypeByExtension might return "type/subtype;param=value".
		// For Content-Type, this is generally acceptable.
		// text/* types often include charset=utf-8, which is good.
		return mimeType
	}

	// 4. Default to application/octet-stream
	return defaultOctetStreamMimeType
}
