package staticfileserver_test

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"example.com/llmahttap/v2/internal/config"
	staticfileserver "example.com/llmahttap/v2/internal/handlers/staticfileserver"
)

// Helper function to create a temporary file with given content
func createTempMimeFile(t *testing.T, content string) string {
	t.Helper()
	tmpFile, err := os.CreateTemp(t.TempDir(), "mime_test_*.json")
	if err != nil {
		t.Fatalf("Failed to create temp file: %v", err)
	}
	if content != "" { // Allow creating an empty file if content is empty string
		if _, err := tmpFile.WriteString(content); err != nil {
			tmpFile.Close()
			os.Remove(tmpFile.Name())
			t.Fatalf("Failed to write to temp file: %v", err)
		}
	}
	if err := tmpFile.Close(); err != nil {
		os.Remove(tmpFile.Name()) // Attempt to clean up even on close error
		t.Fatalf("Failed to close temp file: %v", err)
	}
	return tmpFile.Name()
}

func TestLoadCustomMimeTypesFromFile(t *testing.T) {
	tests := []struct {
		name          string
		fileContent   string
		createFile    bool
		expectError   bool
		errorContains string
		expectedMap   map[string]string
	}{
		{
			name:        "valid json file",
			fileContent: `{".txt": "text/plain", ".JPG": "image/jpeg", ".customExt": "application/x-custom"}`,
			createFile:  true,
			expectError: false,
			expectedMap: map[string]string{
				".txt":       "text/plain",
				".jpg":       "image/jpeg", // Keys are lowercased
				".customext": "application/x-custom",
			},
		},
		{
			name:          "malformed json",
			fileContent:   `{"txt": "text/plain", missing_quote: "foo"`,
			createFile:    true,
			expectError:   true,
			errorContains: "failed to parse JSON",
		},
		{
			name:          "invalid extension format - no dot",
			fileContent:   `{"txt": "text/plain"}`,
			createFile:    true,
			expectError:   true,
			errorContains: "invalid extension \"txt\"",
		},
		{
			name:          "invalid extension format - multiple dots but not at start",
			fileContent:   `{"file.txt": "text/plain"}`,
			createFile:    true,
			expectError:   true,
			errorContains: "invalid extension \"file.txt\"",
		},
		{
			name:          "empty mime type value",
			fileContent:   `{".txt": ""}`,
			createFile:    true,
			expectError:   true,
			errorContains: "empty MIME type for extension \".txt\"",
		},
		{
			name:          "non-existent file",
			fileContent:   "",
			createFile:    false,
			expectError:   true,
			errorContains: "failed to read MIME types file",
		},
		{
			name:        "empty json object file",
			fileContent: `{}`,
			createFile:  true,
			expectError: false,
			expectedMap: map[string]string{},
		},
		{
			name:          "completely empty file (0 bytes)",
			fileContent:   ``, // Explicitly empty content for the file
			createFile:    true,
			expectError:   true,
			errorContains: "failed to parse JSON", // json.Unmarshal(nil, &v) or json.Unmarshal([]byte(""), &v) errors
		},
		{
			name:        "extension with only dot",
			fileContent: `{"." : "application/octet-stream"}`,
			createFile:  true,
			expectError: false,
			expectedMap: map[string]string{
				".": "application/octet-stream",
			},
		},
		{
			name:        "keys are lowercased in result",
			fileContent: `{".TXT": "text/plain", ".JPeG": "image/jpeg"}`,
			createFile:  true,
			expectError: false,
			expectedMap: map[string]string{
				".txt":  "text/plain",
				".jpeg": "image/jpeg",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var filePath string
			if tt.createFile {
				filePath = createTempMimeFile(t, tt.fileContent)
				defer os.Remove(filePath)
			} else {
				filePath = filepath.Join(t.TempDir(), "non_existent_mime_file.json")
			}

			customMap, err := staticfileserver.LoadCustomMimeTypesFromFile(filePath)

			if tt.expectError {
				if err == nil {
					t.Errorf("LoadCustomMimeTypesFromFile() error = nil, wantErr true")
					return
				}
				if tt.errorContains != "" && !strings.Contains(err.Error(), tt.errorContains) {
					t.Errorf("LoadCustomMimeTypesFromFile() error = %q, want error containing %q", err.Error(), tt.errorContains)
				}
			} else {
				if err != nil {
					t.Errorf("LoadCustomMimeTypesFromFile() error = %v, wantErr false", err)
					return
				}
				if !reflect.DeepEqual(customMap, tt.expectedMap) {
					t.Errorf("LoadCustomMimeTypesFromFile() got = %v, want %v", customMap, tt.expectedMap)
				}
			}
		})
	}
}

func TestNewMimeTypeResolver(t *testing.T) {
	mainConfigDir := t.TempDir()
	mainConfigPath := filepath.Join(mainConfigDir, "server.conf")

	tests := []struct {
		name                string
		sfsConfigSetup      func(t *testing.T) *config.StaticFileServerConfig // Function to set up config, allowing temp file creation
		mainConfigFilePath  string
		expectedCustomTypes map[string]string
		expectError         bool
		errorMsgContains    string
	}{
		{
			name: "no custom types",
			sfsConfigSetup: func(t *testing.T) *config.StaticFileServerConfig {
				return &config.StaticFileServerConfig{}
			},
			mainConfigFilePath:  mainConfigPath,
			expectedCustomTypes: map[string]string{},
		},
		{
			name: "only inline map",
			sfsConfigSetup: func(t *testing.T) *config.StaticFileServerConfig {
				return &config.StaticFileServerConfig{
					MimeTypesMap: map[string]string{".custom": "app/custom", ".UPPER": "app/upper"},
				}
			},
			mainConfigFilePath:  mainConfigPath,
			expectedCustomTypes: map[string]string{".custom": "app/custom", ".upper": "app/upper"},
		},
		{
			name: "only file map - absolute path",
			sfsConfigSetup: func(t *testing.T) *config.StaticFileServerConfig {
				absPath := createTempMimeFile(t, `{".filetype": "app/file", ".ANOTHER": "app/another"}`)
				return &config.StaticFileServerConfig{MimeTypesPath: &absPath}
			},
			mainConfigFilePath:  mainConfigPath,
			expectedCustomTypes: map[string]string{".filetype": "app/file", ".another": "app/another"},
		},
		{
			name: "only file map - relative path",
			sfsConfigSetup: func(t *testing.T) *config.StaticFileServerConfig {
				relPath := "custom_mimes.json"
				absPath := filepath.Join(mainConfigDir, relPath) // mainConfigDir from outer scope
				err := os.WriteFile(absPath, []byte(`{".relfile": "app/rel", ".RELTWO": "app/reltwo"}`), 0644)
				if err != nil {
					t.Fatalf("Failed to write relative mime file: %v", err)
				}
				return &config.StaticFileServerConfig{MimeTypesPath: &relPath}
			},
			mainConfigFilePath:  mainConfigPath,
			expectedCustomTypes: map[string]string{".relfile": "app/rel", ".reltwo": "app/reltwo"},
		},
		{
			name: "both inline and file map - file overrides inline",
			sfsConfigSetup: func(t *testing.T) *config.StaticFileServerConfig {
				filePath := createTempMimeFile(t, `{".filetype": "app/file", ".SHARED": "app/file_shared"}`)
				return &config.StaticFileServerConfig{
					MimeTypesMap:  map[string]string{".custom": "app/inline_custom", ".shared": "app/inline_shared"},
					MimeTypesPath: &filePath,
				}
			},
			mainConfigFilePath: mainConfigPath,
			expectedCustomTypes: map[string]string{
				".custom":   "app/inline_custom",
				".filetype": "app/file",
				".shared":   "app/file_shared",
			},
		},
		{
			name: "file map - non-existent file",
			sfsConfigSetup: func(t *testing.T) *config.StaticFileServerConfig {
				p := filepath.Join(t.TempDir(), "nonexistent.json") // Ensure it doesn't exist
				return &config.StaticFileServerConfig{MimeTypesPath: &p}
			},
			mainConfigFilePath: mainConfigPath,
			expectError:        true,
			errorMsgContains:   "failed to read custom MIME types file",
		},
		{
			name: "file map - malformed json in file",
			sfsConfigSetup: func(t *testing.T) *config.StaticFileServerConfig {
				p := createTempMimeFile(t, `{".bad": "app/bad",`)
				return &config.StaticFileServerConfig{MimeTypesPath: &p}
			},
			mainConfigFilePath: mainConfigPath,
			expectError:        true,
			errorMsgContains:   "failed to parse custom MIME types JSON file",
		},

		{
			name: "file map - invalid extension (no dot) in file",
			sfsConfigSetup: func(t *testing.T) *config.StaticFileServerConfig {
				p := createTempMimeFile(t, `{"nodot": "app/invalid"}`)
				return &config.StaticFileServerConfig{MimeTypesPath: &p}
			},
			mainConfigFilePath: mainConfigPath,
			expectError:        true,
			errorMsgContains:   "invalid extension \"nodot\" in custom MIME types file: must start with a '.'",
		},
		{
			name: "file map - empty mime type in file",
			sfsConfigSetup: func(t *testing.T) *config.StaticFileServerConfig {
				p := createTempMimeFile(t, `{".emptyval": ""}`)
				return &config.StaticFileServerConfig{MimeTypesPath: &p}
			},
			mainConfigFilePath: mainConfigPath,
			expectError:        true,
			errorMsgContains:   "empty MIME type for extension \".emptyval\" in custom MIME types file",
		},
		{
			name: "nil sfsConfig",
			sfsConfigSetup: func(t *testing.T) *config.StaticFileServerConfig {
				return nil
			},
			mainConfigFilePath:  mainConfigPath,
			expectedCustomTypes: map[string]string{}, // No error, empty map
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sfsConfig := tt.sfsConfigSetup(t)
			resolver, err := staticfileserver.NewMimeTypeResolver(sfsConfig, tt.mainConfigFilePath)

			if tt.expectError {
				if err == nil {
					t.Fatalf("NewMimeTypeResolver() expected error, got nil")
				}
				if tt.errorMsgContains != "" && !strings.Contains(err.Error(), tt.errorMsgContains) {
					t.Errorf("NewMimeTypeResolver() error = %q, want error containing %q", err.Error(), tt.errorMsgContains)
				}
			} else {
				if err != nil {
					t.Fatalf("NewMimeTypeResolver() unexpected error: %v", err)
				}
				if resolver == nil {
					t.Fatalf("NewMimeTypeResolver() returned nil resolver without error")
				}
				// Check sfsConfig.ResolvedMimeTypes if sfsConfig is not nil
				// This field is populated by NewMimeTypeResolver for inspection.
				var resolvedTypes map[string]string
				if sfsConfig != nil {
					resolvedTypes = sfsConfig.ResolvedMimeTypes
				} else {
					// If sfsConfig is nil, resolver would have an empty internal map.
					// sfsConfig.ResolvedMimeTypes would not be accessible.
					// We expect an empty map in this case.
					resolvedTypes = make(map[string]string)
				}

				if !reflect.DeepEqual(resolvedTypes, tt.expectedCustomTypes) {
					t.Errorf("ResolvedMimeTypes got = %v, want %v", resolvedTypes, tt.expectedCustomTypes)
				}
			}
		})
	}
}

func TestMimeTypeResolver_GetMimeType(t *testing.T) {
	customFilePath := createTempMimeFile(t, `{".customfile": "application/x-custom-file", ".UPPEREXT": "text/upper"}`)
	defer os.Remove(customFilePath)

	sfsConfig := &config.StaticFileServerConfig{
		MimeTypesMap: map[string]string{
			".custominline": "application/x-custom-inline",
			".MixedCase":    "text/mixed",
		},
		MimeTypesPath: &customFilePath,
	}

	resolver, err := staticfileserver.NewMimeTypeResolver(sfsConfig, "")
	if err != nil {
		t.Fatalf("Failed to create MimeTypeResolver: %v", err)
	}

	tests := []struct {
		name     string
		filePath string
		expected string
	}{
		// Precedence 1: Custom Mappings (from MimeTypeResolver's customMimeTypes, loaded from file or inline config)
		{"P1 custom from file: .customfile", "archive.customfile", "application/x-custom-file"},
		{"P1 custom from file: .UPPEREXT (mixed case in config, lowercase in filename)", "document.upperext", "text/upper"},
		{"P1 custom from file: .UPPEREXT (mixed case in config, uppercase in filename)", "DOCUMENT.UPPEREXT", "text/upper"},
		{"P1 custom from inline map: .custominline", "data.custominline", "application/x-custom-inline"},
		{"P1 custom from inline map: .MixedCase (mixed case in config, lowercase in filename)", "info.mixedcase", "text/mixed"},
		{"P1 custom from inline map: .MixedCase (mixed case in config, uppercase in filename)", "INFO.MIXEDCASE", "text/mixed"},

		// Precedence 2: Go's mime.TypeByExtension
		{"P2 Go's mime: .html", "index.html", "text/html; charset=utf-8"},
		{"P2 Go's mime: .html (uppercase filename)", "INDEX.HTML", "text/html; charset=utf-8"},
		{"P2 Go's mime: .jpeg", "image.jpeg", "image/jpeg"},
		{"P2 Go's mime: .jpg", "image.jpg", "image/jpeg"},
		{"P2 Go's mime: .png", "graphic.png", "image/png"},
		{"P2 Go's mime: .json", "data.json", "application/json"}, // stdlib mime.TypeByExtension(".json") is "application/json"
		{"P2 Go's mime: .js", "script.js", "text/javascript; charset=utf-8"},
		{"P2 Go's mime: .css", "style.css", "text/css; charset=utf-8"},
		{"P2 Go's mime: .aac", "audio.aac", "audio/aac"},                                         // stdlib knows .aac
		{"P2 Go's mime: .webp", "image.webp", "image/webp"},                                      // stdlib knows .webp
		{"P2 Go's mime: .gz (from multi-dot file.tar.gz)", "archive.tar.gz", "application/gzip"}, // filepath.Ext -> .gz

		// Precedence 3: Our package's defaultMimeTypes (when not in custom and not known by Go's mime.TypeByExtension)
		// .azw: Go's mime.TypeByExtension(".azw") returns "", but it's in our defaultMimeTypes
		{"P3 Our defaultMimeTypes: .azw", "ebook.azw", "application/vnd.amazon.ebook"},
		// .xul: Go's mime.TypeByExtension(".xul") returns "", but it's in our defaultMimeTypes
		{"P3 Our defaultMimeTypes: .xul", "app.xul", "application/vnd.mozilla.xul+xml"},

		// Precedence 4: Fallback to application/octet-stream
		{"P4 Fallback: .unknownextension", "file.unknownextension", "application/octet-stream"},
		{"P4 Fallback: no extension", "filewithoutextension", "application/octet-stream"},
		{"P4 Fallback: only dot in filename", ".", "application/octet-stream"},               // filepath.Ext(".") is "."
		{"P4 Fallback: filename ends with dot", "archive.tar.", "application/octet-stream"},  // filepath.Ext("archive.tar.") is "."
		{"P4 Fallback: hidden file no specific type", ".bashrc", "application/octet-stream"}, // filepath.Ext(".bashrc") is ".bashrc", assume not known
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolver.GetMimeType(tt.filePath)
			if got != tt.expected {
				t.Errorf("GetMimeType(%q) = %q, want %q", tt.filePath, got, tt.expected)
			}
		})
	}
}

func TestResolveMimeType(t *testing.T) {
	customUserMappings := map[string]string{
		".custom": "application/x-custom",
		".mixed":  "text/mixed-case", // Ensure key is lowercase
	}

	tests := []struct {
		name               string
		extension          string
		customUserMappings map[string]string
		expected           string
	}{
		{"custom mapping", ".custom", customUserMappings, "application/x-custom"},
		{"custom mapping - mixed case ext input", ".MIXED", customUserMappings, "text/mixed-case"},
		{"custom mapping - not found", ".foo", customUserMappings, "application/octet-stream"}, // .foo not in custom, default, or Go's (assumed)

		{"default mapping - txt", ".txt", nil, "text/plain; charset=utf-8"},
		{"default mapping - webp", ".webp", customUserMappings, "image/webp"}, // .webp not in custom for this test

		{"go's mime - jpeg", ".jpeg", nil, "image/jpeg"},

		{"unknown extension", ".unknown", nil, "application/octet-stream"},
		{"empty extension", "", nil, "application/octet-stream"},
		{"only dot extension", ".", nil, "application/octet-stream"},

		{
			name:               "precedence: custom overrides default",
			extension:          ".txt",
			customUserMappings: map[string]string{".txt": "text/custom-plain"},
			expected:           "text/custom-plain",
		},
		{
			name:               "precedence: default considered if custom nil",
			extension:          ".avif", // In our default list
			customUserMappings: nil,
			expected:           "image/avif",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := staticfileserver.ResolveMimeType(tt.extension, tt.customUserMappings)
			if got != tt.expected {
				t.Errorf("ResolveMimeType(%q) = %q, want %q", tt.extension, got, tt.expected)
			}
		})
	}
}
