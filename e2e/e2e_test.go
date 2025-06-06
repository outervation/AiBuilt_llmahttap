//go:build unix

package e2e

import (
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time" // Ensure time is here

	"example.com/llmahttap/v2/e2e/testutil"
	"example.com/llmahttap/v2/internal/config"
)

var serverBinaryPath string
var serverBinaryMissing bool // Flag to indicate if server binary is missing/invalid
var defaultCurlPath = "curl" // Default path, can be overridden by environment

func init() {
	serverBinaryPath = os.Getenv("SERVER_BINARY_PATH")
	if serverBinaryPath == "" {
		projRoot, err := filepath.Abs("../../")
		if err == nil {
			candidates := []string{
				filepath.Join(projRoot, "server"),
				filepath.Join(projRoot, "llmahttapv2"),
				filepath.Join(projRoot, "cmd", "server", "server"),
				filepath.Join(projRoot, "cmd", "server", "main"),
				filepath.Join(projRoot, "build", "server"),
				filepath.Join(projRoot, "build", "llmahttapv2_server"),
			}
			for _, candidate := range candidates {
				if _, statErr := os.Stat(candidate); statErr == nil {
					serverBinaryPath = candidate
					fmt.Printf("E2E Info: Automatically found server binary at: %s\n", serverBinaryPath)
					break
				}
			}
		}
	}
	if serverBinaryPath == "" {
		serverBinaryPath = "../../llmahttapv2_server" // Default path if not overridden or found
		fmt.Printf("E2E Info: SERVER_BINARY_PATH not set and auto-detection failed. Using default: %s\n", serverBinaryPath)
	}

	info, err := os.Stat(serverBinaryPath)
	if os.IsNotExist(err) {
		fmt.Printf("E2E Warning: Server binary not found at '%s'. E2E tests will be skipped.\n", serverBinaryPath)
		serverBinaryMissing = true
	} else if err != nil {
		fmt.Printf("E2E Warning: Error accessing server binary at '%s': %v. E2E tests will be skipped.\n", serverBinaryPath, err)
		serverBinaryMissing = true
	} else if info.IsDir() || (info.Mode()&0111 == 0) {
		fmt.Printf("E2E Warning: Server binary path '%s' is a directory or not executable. E2E tests will be skipped.\n", serverBinaryPath)
		serverBinaryMissing = true
	}

	if val := os.Getenv("CURL_PATH"); val != "" {
	}
}

// Helper function to get a pointer to a string.
func strPtr(s string) *string {
	return &s
}

// Helper function to get a pointer to a boolean.
func boolPtr(b bool) *bool {
	return &b
}

// setupTempFiles creates a temporary document root and populates it
// with files and their content as specified in fileMap.
// fileMap keys are relative paths within the docRoot, values are file contents.
// Used by static file tests and potentially others.
func setupTempFiles(t *testing.T, fileMap map[string]string) (docRoot string, cleanupFunc func(), err error) {
	t.Helper()
	docRoot, err = os.MkdirTemp("", "e2e-docroot-*")
	if err != nil {
		return "", nil, fmt.Errorf("failed to create temp docroot: %w", err)
	}

	cleanupFunc = func() {
		os.RemoveAll(docRoot)
	}

	for relPath, content := range fileMap {
		fullPath := filepath.Join(docRoot, relPath)
		dir := filepath.Dir(fullPath)
		if err := os.MkdirAll(dir, 0755); err != nil {
			cleanupFunc() // Clean up already created docRoot
			return "", nil, fmt.Errorf("failed to create directory %s: %w", dir, err)
		}
		if err := os.WriteFile(fullPath, []byte(content), 0644); err != nil {
			cleanupFunc()
			return "", nil, fmt.Errorf("failed to write file %s: %w", fullPath, err)
		}
	}
	return docRoot, cleanupFunc, nil
}

// TeardownServer is a placeholder for explicit server teardown logic if needed
// beyond what testutil.ServerInstance.Stop() provides.
// For now, testutil.ServerInstance.Stop() should be sufficient.
func TeardownServer(serverInstance *testutil.ServerInstance) {
	if serverInstance != nil {
		if err := serverInstance.Stop(); err != nil {
			// t.Logf is not available here, so print to stdout/stderr
			fmt.Printf("Error stopping server instance during TeardownServer: %v\nServer Logs:\n%s\n", err, serverInstance.SafeGetLogs())
		}
	}
}

// Note: We deliberately don't use a TestMain here, as the harness framework
// can't distinguish between a TestMain failure and a unit test compilation failure,
// because running with all tests disabled (just to check the tests build) still
// runs the TestMains.

// TestPlaceholder is a basic test to ensure the E2E setup works.
// It starts the server with a minimal config and makes a single request.
func TestPlaceholder(t *testing.T) {
	t.Parallel()
	tempDir, err := os.MkdirTemp("", "placeholder-docroot")
	if serverBinaryMissing {
		t.Skipf("Skipping E2E test: server binary not found or not executable at '%s'", serverBinaryPath)
	}
	if err != nil {
		t.Fatalf("Failed to create temp dir for placeholder test: %v", err)
	}
	defer os.RemoveAll(tempDir)

	baseConfig := &config.Config{
		Server: &config.ServerConfig{
			Address: strPtr("127.0.0.1:0"), // Dynamic port
		},
		Routing: &config.RoutingConfig{
			Routes: []config.Route{
				{
					PathPattern: "/",
					MatchType:   config.MatchTypeExact,
					HandlerType: "StaticFileServer", // Needs a dummy file or will 404
					HandlerConfig: testutil.ToRawMessageWrapper(t, config.StaticFileServerConfig{
						DocumentRoot:          tempDir, // Use temp dir
						ServeDirectoryListing: boolPtr(false),
					}),
				},
			},
		},
		Logging: &config.LoggingConfig{
			LogLevel: config.LogLevelDebug,
			AccessLog: &config.AccessLogConfig{
				Enabled: boolPtr(true),
				Target:  strPtr("stdout"),
				Format:  "json",
			},
			ErrorLog: &config.ErrorLogConfig{
				Target: strPtr("stdout"),
			},
		},
	}

	testDef := testutil.E2ETestDefinition{
		Name:                "PlaceholderTest",
		ServerBinaryPath:    serverBinaryPath,
		ServerConfigData:    baseConfig,
		ServerConfigFormat:  "json",
		ServerConfigArgName: "-config",
		ServerListenAddress: *baseConfig.Server.Address, // Used for initial polling
		TestCases: []testutil.E2ETestCase{
			{
				Name: "GET Root (expect 404)",
				Request: testutil.TestRequest{
					Method: "GET",
					Path:   "/",
				},
				Expected: testutil.ExpectedResponse{
					StatusCode: http.StatusNotFound, // Expecting 404 as no index.html and listing disabled
					Headers:    testutil.HeaderMatcher{"content-type": "application/json; charset=utf-8"},
					BodyMatcher: &testutil.JSONFieldsBodyMatcher{
						ExpectedFields: map[string]interface{}{
							"error": map[string]interface{}{
								"status_code": 404.0, // JSON numbers are float64
								"message":     "Not Found",
							},
						},
					},
				},
			},
		},
		CurlPath: defaultCurlPath,
	}
	testutil.RunE2ETest(t, testDef)
}

// TestDefaultErrorResponses verifies the server's default error response generation.
func TestDefaultErrorResponses(t *testing.T) {
	t.Parallel()
	if serverBinaryMissing {
		t.Skipf("Skipping E2E test: server binary not found or not executable at '%s'", serverBinaryPath)
	}
	baseConfig := &config.Config{
		Server: &config.ServerConfig{
			Address: strPtr("127.0.0.1:0"),
		},
		Routing: &config.RoutingConfig{
			Routes: []config.Route{}, // No routes configured, so all requests should 404
		},
		Logging: &config.LoggingConfig{
			LogLevel: config.LogLevelDebug,
			AccessLog: &config.AccessLogConfig{
				Enabled: boolPtr(true), Target: strPtr("stdout"), Format: "json",
			},
			ErrorLog: &config.ErrorLogConfig{Target: strPtr("stdout")},
		},
	}

	testCases := []testutil.E2ETestCase{
		{
			Name: "GET NonExistent Path - Expect JSON",
			Request: testutil.TestRequest{
				Method:  "GET",
				Path:    "/this/path/does/not/exist",
				Headers: http.Header{"Accept": []string{"application/json, text/html;q=0.9"}},
			},
			Expected: testutil.ExpectedResponse{
				StatusCode: http.StatusNotFound,
				Headers:    testutil.HeaderMatcher{"content-type": "application/json; charset=utf-8"},
				BodyMatcher: &testutil.JSONFieldsBodyMatcher{
					ExpectedFields: map[string]interface{}{
						"error": map[string]interface{}{
							"status_code": 404.0,
							"message":     "Not Found",
						},
					},
				},
			},
		},
		{
			Name: "GET NonExistent Path - Expect HTML by default",
			Request: testutil.TestRequest{
				Method: "GET",
				Path:   "/this/path/does/not/exist/either",
				// No Accept header, or Accept: text/html
			},
			Expected: testutil.ExpectedResponse{
				StatusCode: http.StatusNotFound,
				Headers:    testutil.HeaderMatcher{"content-type": "text/html; charset=utf-8"},
				BodyMatcher: &testutil.StringContainsBodyMatcher{
					Substring: "<h1>Not Found</h1>",
				},
			},
		},
		{
			Name: "GET NonExistent Path - Expect HTML with text/*",
			Request: testutil.TestRequest{
				Method:  "GET",
				Path:    "/another/nonexistent/path",
				Headers: http.Header{"Accept": []string{"text/*"}},
			},
			Expected: testutil.ExpectedResponse{
				StatusCode: http.StatusNotFound,
				Headers:    testutil.HeaderMatcher{"content-type": "text/html; charset=utf-8"},
				BodyMatcher: &testutil.StringContainsBodyMatcher{
					Substring: "<p>The requested resource was not found on this server.</p>",
				},
			},
		},
		// Add more cases for other errors like 405, 500 (if triggerable easily)
		// For 405, we'd need a route that exists but doesn't support a method.
		// For 500, it's harder to trigger reliably in E2E without a specific handler.
	}

	testDef := testutil.E2ETestDefinition{
		Name:                "DefaultErrorResponses",
		ServerBinaryPath:    serverBinaryPath,
		ServerConfigData:    baseConfig,
		ServerConfigFormat:  "json",
		ServerConfigArgName: "-config",
		ServerListenAddress: *baseConfig.Server.Address,
		TestCases:           testCases,
		CurlPath:            defaultCurlPath,
	}
	testutil.RunE2ETest(t, testDef)
}

// TestRouting_Basic is a placeholder for basic routing tests.
func TestRouting_Basic(t *testing.T) {
	// TODO: Implement basic routing tests:
	// - Exact match
	// - Prefix match
	// - No match (404)
	// - Simple handlers (e.g., a mock handler that returns fixed response)
	t.Skip("TestRouting_Basic not yet implemented")
}

// TestStaticFileServing is a placeholder for comprehensive static file serving tests.
func TestStaticFileServing(t *testing.T) {
	t.Run("BasicFileOperations", TestStaticFileServer_BasicFileOperations)
	t.Run("DirectoryListing", TestStaticFileServer_DirectoryListing)
	t.Run("IndexFiles", TestStaticFileServer_IndexFiles)
	t.Run("ConditionalRequests", TestStaticFileServer_ConditionalRequests)
	t.Run("MimeTypes", TestStaticFileServer_MimeTypes)
	// Add other sub-tests for static file serving here
}

// TestStaticFileServer_BasicFileOperations covers fundamental file serving aspects.
func TestStaticFileServer_BasicFileOperations(t *testing.T) {
	t.Parallel()

	if serverBinaryMissing {
		t.Skipf("Skipping E2E test: server binary not found or not executable at '%s'", serverBinaryPath)
	}
	docRoot, cleanupDocRoot, err := setupTempFiles(t, map[string]string{
		"hello.txt":         "Hello, world!",
		"another file.html": "<html><body>Test</body></html>", // File with space in name
		"empty.dat":         "",
	})
	if err != nil {
		t.Fatalf("Failed to set up temp files: %v", err)
	}
	defer cleanupDocRoot()

	cfg := &config.Config{
		Server: &config.ServerConfig{
			Address: strPtr("127.0.0.1:0"),
		},
		Routing: &config.RoutingConfig{
			Routes: []config.Route{
				{
					PathPattern: "/static/",
					MatchType:   config.MatchTypePrefix,
					HandlerType: "StaticFileServer",
					HandlerConfig: testutil.ToRawMessageWrapper(t, config.StaticFileServerConfig{
						DocumentRoot:          docRoot,
						ServeDirectoryListing: boolPtr(false), // Keep it simple for basic ops
					}),
				},
			},
		},
		Logging: &config.LoggingConfig{LogLevel: config.LogLevelDebug, AccessLog: &config.AccessLogConfig{Enabled: boolPtr(false)}},
	}

	testCases := []testutil.E2ETestCase{
		{
			Name: "GET File (hello.txt)",
			Request: testutil.TestRequest{
				Method: "GET",
				Path:   "/static/hello.txt",
			},
			Expected: testutil.ExpectedResponse{
				StatusCode: http.StatusOK,
				Headers: testutil.HeaderMatcher{
					"content-type": "text/plain; charset=utf-8", // Default for .txt
					// Content-Length, Last-Modified, ETag are harder to predict exactly without file system interaction here
					// We will rely on the server setting them and curl/go client receiving them.
					// A more robust test would stat the file and compute expected values.
				},
				BodyMatcher: &testutil.ExactBodyMatcher{ExpectedBody: []byte("Hello, world!")},
			},
		},
		{
			Name: "HEAD File (hello.txt)",
			Request: testutil.TestRequest{
				Method: "HEAD",
				Path:   "/static/hello.txt",
			},
			Expected: testutil.ExpectedResponse{
				StatusCode: http.StatusOK,
				Headers: testutil.HeaderMatcher{
					"content-type": "text/plain; charset=utf-8",
					// Expect Content-Length to be present and non-zero
				},
				ExpectNoBody: true,
			},
		},
		{
			Name: "GET File with space in name (another file.html)",
			Request: testutil.TestRequest{
				Method: "GET",
				Path:   "/static/another%20file.html", // URL encoded space
			},
			Expected: testutil.ExpectedResponse{
				StatusCode:  http.StatusOK,
				Headers:     testutil.HeaderMatcher{"content-type": "text/html; charset=utf-8"},
				BodyMatcher: &testutil.ExactBodyMatcher{ExpectedBody: []byte("<html><body>Test</body></html>")},
			},
		},
		{
			Name: "GET Empty File (empty.dat)",
			Request: testutil.TestRequest{
				Method: "GET",
				Path:   "/static/empty.dat",
			},
			Expected: testutil.ExpectedResponse{
				StatusCode:  http.StatusOK,
				Headers:     testutil.HeaderMatcher{"content-type": "application/octet-stream", "content-length": "0"},
				BodyMatcher: &testutil.ExactBodyMatcher{ExpectedBody: []byte("")},
			},
		},
		{
			Name: "OPTIONS File Path (hello.txt)",
			Request: testutil.TestRequest{
				Method: "OPTIONS",
				Path:   "/static/hello.txt",
			},
			Expected: testutil.ExpectedResponse{
				StatusCode: http.StatusNoContent, // Or 200 OK with Allow
				Headers:    testutil.HeaderMatcher{"allow": "GET, HEAD, OPTIONS"},
				// Body should be empty for 204. If 200, could have a body but usually not for OPTIONS.
			},
		},
		{
			Name: "Path Traversal Attempt",
			Request: testutil.TestRequest{
				Method: "GET",
				Path:   "/static/../secret.txt", // Attempt to go up one level
			},
			Expected: testutil.ExpectedResponse{
				StatusCode: http.StatusNotFound, // Spec 2.3.1 says 404
				Headers:    testutil.HeaderMatcher{"content-type": "application/json; charset=utf-8"},
			},
		},
		{
			Name: "Path Traversal Attempt (Encoded)",
			Request: testutil.TestRequest{
				Method: "GET",
				Path:   "/static/%2E%2E/secret.txt", // Encoded ../
			},
			Expected: testutil.ExpectedResponse{
				StatusCode: http.StatusNotFound,
				Headers:    testutil.HeaderMatcher{"content-type": "application/json; charset=utf-8"},
			},
		},
		{
			Name: "File Not Found (within DocRoot)",
			Request: testutil.TestRequest{
				Method: "GET",
				Path:   "/static/nonexistentfile.txt",
			},
			Expected: testutil.ExpectedResponse{
				StatusCode: http.StatusNotFound,
				Headers:    testutil.HeaderMatcher{"content-type": "application/json; charset=utf-8"},
			},
		},
		{
			Name: "Unsupported Method (POST to file)",
			Request: testutil.TestRequest{
				Method: "POST",
				Path:   "/static/hello.txt",
				Body:   []byte("some data"),
			},
			Expected: testutil.ExpectedResponse{
				StatusCode: http.StatusMethodNotAllowed,
				Headers:    testutil.HeaderMatcher{"content-type": "application/json; charset=utf-8"},
			},
		},
	}

	testDef := testutil.E2ETestDefinition{
		Name:                "StaticFileServer_BasicFileOperations",
		ServerBinaryPath:    serverBinaryPath,
		ServerConfigData:    cfg,
		ServerConfigFormat:  "json",
		ServerConfigArgName: "-config",
		ServerListenAddress: *cfg.Server.Address,
		TestCases:           testCases,
		CurlPath:            defaultCurlPath,
	}
	testutil.RunE2ETest(t, testDef)
}

// TestStaticFileServer_DirectoryListing is a placeholder.
func TestStaticFileServer_DirectoryListing(t *testing.T) {
	t.Parallel() // Main test can be parallel

	if serverBinaryMissing {
		t.Skipf("Skipping E2E test: server binary not found or not executable at '%s'", serverBinaryPath)
	}

	// Helper function to create a base config for static file tests focusing on directory operations
	getBaseDirTestConfig := func(st *testing.T, docRoot string, indexFiles []string, serveListing *bool) *config.Config {
		st.Helper()
		staticHandlerCfg := config.StaticFileServerConfig{
			DocumentRoot:          docRoot,
			IndexFiles:            indexFiles,
			ServeDirectoryListing: serveListing,
		}
		return &config.Config{
			Server: &config.ServerConfig{
				Address: strPtr("127.0.0.1:0"),
			},
			Routing: &config.RoutingConfig{
				Routes: []config.Route{
					{
						PathPattern:   "/static/",
						MatchType:     config.MatchTypePrefix,
						HandlerType:   "StaticFileServer",
						HandlerConfig: testutil.ToRawMessageWrapper(st, staticHandlerCfg),
					},
				},
			},
			Logging: &config.LoggingConfig{
				LogLevel:  config.LogLevelError, // Keep logs quieter for these tests
				AccessLog: &config.AccessLogConfig{Enabled: boolPtr(false)},
				ErrorLog:  &config.ErrorLogConfig{Target: strPtr("stderr")}, // So testutil can capture it if server fails
			},
		}
	}

	// Sub-test 1: Default Index File (`index.html`)
	t.Run("DefaultIndexFile", func(st *testing.T) {
		st.Parallel()
		docRoot, cleanupDocRoot, err := setupTempFiles(st, map[string]string{
			"index.html": "Default Index Page Content",
		})
		if err != nil {
			st.Fatalf("Failed to set up temp files: %v", err)
		}
		defer cleanupDocRoot()

		// Explicitly set IndexFiles to default or let server default it. Spec 2.2.2 defaults to ["index.html"].
		// ServeDirectoryListing default is false (Spec 2.2.3).
		cfg := getBaseDirTestConfig(st, docRoot, nil /* defaults to ["index.html"] */, nil /* defaults to false */)

		testDef := testutil.E2ETestDefinition{
			Name:                "Static_DefaultIndexFile",
			ServerBinaryPath:    serverBinaryPath,
			ServerConfigData:    cfg,
			ServerConfigFormat:  "json",
			ServerConfigArgName: "-config",
			ServerListenAddress: *cfg.Server.Address,
			TestCases: []testutil.E2ETestCase{
				{
					Name:    "GET /static/ (serves index.html)",
					Request: testutil.TestRequest{Method: "GET", Path: "/static/"},
					Expected: testutil.ExpectedResponse{
						StatusCode:  http.StatusOK,
						Headers:     testutil.HeaderMatcher{"content-type": "text/html; charset=utf-8"},
						BodyMatcher: &testutil.ExactBodyMatcher{ExpectedBody: []byte("Default Index Page Content")},
					},
				},
			},
			CurlPath: defaultCurlPath,
		}
		testutil.RunE2ETest(st, testDef)
	})

	// Sub-test 2: Custom Index File (`custom.html` takes precedence)
	t.Run("CustomIndexFile", func(st *testing.T) {
		st.Parallel()
		docRoot, cleanupDocRoot, err := setupTempFiles(st, map[string]string{
			"custom.html": "Custom Page Served",
			"index.html":  "Default Index, Should Not Be Served",
		})
		if err != nil {
			st.Fatalf("Failed to set up temp files: %v", err)
		}
		defer cleanupDocRoot()

		cfg := getBaseDirTestConfig(st, docRoot, []string{"custom.html", "index.html"}, boolPtr(false))

		testDef := testutil.E2ETestDefinition{
			Name:                "Static_CustomIndexFile",
			ServerBinaryPath:    serverBinaryPath,
			ServerConfigData:    cfg,
			ServerConfigFormat:  "json",
			ServerConfigArgName: "-config",
			ServerListenAddress: *cfg.Server.Address,
			TestCases: []testutil.E2ETestCase{
				{
					Name:    "GET /static/ (serves custom.html)",
					Request: testutil.TestRequest{Method: "GET", Path: "/static/"},
					Expected: testutil.ExpectedResponse{
						StatusCode:  http.StatusOK,
						Headers:     testutil.HeaderMatcher{"content-type": "text/html; charset=utf-8"},
						BodyMatcher: &testutil.ExactBodyMatcher{ExpectedBody: []byte("Custom Page Served")},
					},
				},
			},
			CurlPath: defaultCurlPath,
		}
		testutil.RunE2ETest(st, testDef)
	})

	// Sub-test 3: Directory Listing Enabled (No Index Files Match)
	t.Run("ListingEnabledNoIndex", func(st *testing.T) {
		st.Parallel()
		docRoot, cleanupDocRoot, err := setupTempFiles(st, map[string]string{
			"file1.txt":         "content of file1",
			"another_dir/":      "", // Just creating the directory
			"another_dir/f2.md": "content of f2",
		})
		if err != nil {
			st.Fatalf("Failed to set up temp files: %v", err)
		}
		defer cleanupDocRoot()

		// No matching index files, ServeDirectoryListing = true
		cfg := getBaseDirTestConfig(st, docRoot, []string{"nonexistent.idx"}, boolPtr(true))

		testCases := []testutil.E2ETestCase{
			{
				Name:    "GET /static/ (serves listing)",
				Request: testutil.TestRequest{Method: "GET", Path: "/static/"},
				Expected: testutil.ExpectedResponse{
					StatusCode:  http.StatusOK,
					Headers:     testutil.HeaderMatcher{"content-type": "text/html; charset=utf-8"},
					BodyMatcher: &testutil.StringContainsBodyMatcher{Substring: "<a href=\"file1.txt\">file1.txt</a>"},
				},
			},
			{
				Name:    "GET /static/ (listing contains another_dir)",
				Request: testutil.TestRequest{Method: "GET", Path: "/static/"},
				Expected: testutil.ExpectedResponse{
					StatusCode:  http.StatusOK,
					BodyMatcher: &testutil.StringContainsBodyMatcher{Substring: "<a href=\"another_dir/\">another_dir/</a>"},
				},
			},
			{
				Name:    "GET /static/another_dir/ (serves listing for subdir)",
				Request: testutil.TestRequest{Method: "GET", Path: "/static/another_dir/"},
				Expected: testutil.ExpectedResponse{
					StatusCode:  http.StatusOK,
					Headers:     testutil.HeaderMatcher{"content-type": "text/html; charset=utf-8"},
					BodyMatcher: &testutil.StringContainsBodyMatcher{Substring: "<a href=\"f2.md\">f2.md</a>"},
				},
			},
			{
				Name:    "GET /static/another_dir/ (listing contains parent link)",
				Request: testutil.TestRequest{Method: "GET", Path: "/static/another_dir/"},
				Expected: testutil.ExpectedResponse{
					StatusCode:  http.StatusOK,
					BodyMatcher: &testutil.StringContainsBodyMatcher{Substring: "<a href=\"../\">../</a>"},
				},
			},
			{
				Name: "HEAD /static/ (for directory listing)",
				Request: testutil.TestRequest{
					Method: "HEAD",
					Path:   "/static/",
				},
				Expected: testutil.ExpectedResponse{
					StatusCode:   http.StatusOK,
					Headers:      testutil.HeaderMatcher{"content-type": "text/html; charset=utf-8"},
					ExpectNoBody: true,
				},
			},
		}
		testDef := testutil.E2ETestDefinition{
			Name:                "Static_ListingEnabledNoIndex",
			ServerBinaryPath:    serverBinaryPath,
			ServerConfigData:    cfg,
			ServerConfigFormat:  "json",
			ServerConfigArgName: "-config",
			ServerListenAddress: *cfg.Server.Address,
			TestCases:           testCases,
			CurlPath:            defaultCurlPath,
		}
		testutil.RunE2ETest(st, testDef)
	})

	// Sub-test 4: Directory Listing Disabled (No Index Files Match, 403 Forbidden)
	t.Run("ListingDisabledForbidden", func(st *testing.T) {
		st.Parallel()
		docRoot, cleanupDocRoot, err := setupTempFiles(st, map[string]string{
			"somefile.txt": "content",
		})
		if err != nil {
			st.Fatalf("Failed to set up temp files: %v", err)
		}
		defer cleanupDocRoot()

		// No matching index files, ServeDirectoryListing = false (explicitly or default)
		cfg := getBaseDirTestConfig(st, docRoot, []string{"nonexistent.idx"}, boolPtr(false))

		testDef := testutil.E2ETestDefinition{
			Name:                "Static_ListingDisabledForbidden",
			ServerBinaryPath:    serverBinaryPath,
			ServerConfigData:    cfg,
			ServerConfigFormat:  "json",
			ServerConfigArgName: "-config",
			ServerListenAddress: *cfg.Server.Address,
			TestCases: []testutil.E2ETestCase{
				{
					Name: "GET /static/ (expect 403 JSON)",
					Request: testutil.TestRequest{
						Method:  "GET",
						Path:    "/static/",
						Headers: http.Header{"Accept": []string{"application/json"}},
					},
					Expected: testutil.ExpectedResponse{
						StatusCode: http.StatusForbidden,
						Headers:    testutil.HeaderMatcher{"content-type": "application/json; charset=utf-8"},
						BodyMatcher: &testutil.JSONFieldsBodyMatcher{
							ExpectedFields: map[string]interface{}{
								"error": map[string]interface{}{
									"status_code": 403.0, // JSON numbers are float64
									"message":     "Forbidden",
								},
							},
						},
					},
				},
				{
					Name: "GET /static/ (expect 403 HTML)",
					Request: testutil.TestRequest{
						Method:  "GET",
						Path:    "/static/",
						Headers: http.Header{"Accept": []string{"text/html"}},
					},
					Expected: testutil.ExpectedResponse{
						StatusCode:  http.StatusForbidden,
						Headers:     testutil.HeaderMatcher{"content-type": "text/html; charset=utf-8"},
						BodyMatcher: &testutil.StringContainsBodyMatcher{Substring: "<h1>Forbidden</h1>"},
					},
				},
			},
			CurlPath: defaultCurlPath,
		}
		testutil.RunE2ETest(st, testDef)
	})
}

// TestStaticFileServer_IndexFiles is a placeholder.
func TestStaticFileServer_IndexFiles(t *testing.T) {
	t.Parallel()
	t.Skip("TestStaticFileServer_IndexFiles not yet implemented")
}

// TestStaticFileServer_ConditionalRequests is a placeholder.

func TestStaticFileServer_ConditionalRequests(t *testing.T) {
	t.Parallel()

	if serverBinaryMissing {
		t.Skipf("Skipping E2E test: server binary not found or not executable at '%s'", serverBinaryPath)
	}

	fileName := "cacheable_file.txt"
	fileContent := "This is some content that can be cached."
	docRoot, cleanupDocRoot, err := setupTempFiles(t, map[string]string{
		fileName: fileContent,
	})
	if err != nil {
		t.Fatalf("Failed to set up temp files: %v", err)
	}
	defer cleanupDocRoot()

	cfg := &config.Config{
		Server: &config.ServerConfig{
			Address: strPtr("127.0.0.1:0"),
		},
		Routing: &config.RoutingConfig{
			Routes: []config.Route{
				{
					PathPattern: "/static/",
					MatchType:   config.MatchTypePrefix,
					HandlerType: "StaticFileServer",
					HandlerConfig: testutil.ToRawMessageWrapper(t, config.StaticFileServerConfig{
						DocumentRoot:          docRoot,
						ServeDirectoryListing: boolPtr(false),
					}),
				},
			},
		},
		Logging: &config.LoggingConfig{LogLevel: config.LogLevelDebug, AccessLog: &config.AccessLogConfig{Enabled: boolPtr(false)}},
	}

	configFile, cleanupCfgFile, errCfgFile := testutil.WriteTempConfig(cfg, "json")
	if errCfgFile != nil {
		t.Fatalf("Failed to write temp config for conditional requests test: %v", errCfgFile)
	}
	defer cleanupCfgFile()

	server, err := testutil.StartTestServer(serverBinaryPath, configFile, "-config", *cfg.Server.Address)
	if err != nil {
		logStr := ""
		if server != nil { // server might be nil if StartTestServer fails very early
			logStr = server.SafeGetLogs()
		}
		t.Fatalf("Failed to start server for conditional requests test: %v. Logs: %s", err, logStr)
	}
	defer server.Stop()

	clients := []struct {
		name   string
		client testutil.TestRunner
	}{
		{"GoHTTPClient", testutil.NewGoNetHTTPClient()},
		{"CurlClient", testutil.NewCurlHTTPClient(defaultCurlPath)},
	}

	filePathInServer := "/static/" + fileName

	for _, c := range clients {
		t.Run(c.name, func(st *testing.T) {
			st.Helper()
			st.Cleanup(func() {
				if st.Failed() {
					logs := server.SafeGetLogs()
					st.Logf("BEGIN Server logs for client %s, case ConditionalRequests (on failure):\n%s\nEND Server logs", c.name, logs)
				}
			})

			// 1. Initial GET to fetch ETag and Last-Modified
			initialReq := testutil.TestRequest{Method: "GET", Path: filePathInServer}
			actualResp1, err1 := c.client.Run(server, &initialReq)

			if err1 != nil {
				st.Fatalf("Initial GET request failed: %v", err1)
			}
			testutil.AssertStatusCode(st, testutil.ExpectedResponse{StatusCode: http.StatusOK}, *actualResp1)
			initialETag := actualResp1.Headers.Get("Etag")
			initialLastModified := actualResp1.Headers.Get("Last-Modified")
			if initialETag == "" {
				st.Fatalf("Initial GET response missing ETag header. Headers: %v", actualResp1.Headers)
			}
			if initialLastModified == "" {
				st.Fatalf("Initial GET response missing Last-Modified header. Headers: %v", actualResp1.Headers)
			}
			st.Logf("Initial GET for %s: ETag='%s', Last-Modified='%s'", filePathInServer, initialETag, initialLastModified)

			// 2. If-None-Match: Matching ETag should return 304
			reqIfNoneMatchHit := testutil.TestRequest{
				Method: "GET", Path: filePathInServer,
				Headers: http.Header{"If-None-Match": []string{initialETag}},
			}
			actualRespINMHit, errINMHit := c.client.Run(server, &reqIfNoneMatchHit)
			if errINMHit != nil {
				st.Fatalf("GET with If-None-Match (hit) failed: %v", errINMHit)
			}
			testutil.AssertStatusCode(st, testutil.ExpectedResponse{StatusCode: http.StatusNotModified}, *actualRespINMHit)
			st.Logf("If-None-Match (Hit): Status %d OK", actualRespINMHit.StatusCode)

			// 3. If-None-Match: Non-matching ETag should return 200
			reqIfNoneMatchMiss := testutil.TestRequest{
				Method: "GET", Path: filePathInServer,
				Headers: http.Header{"If-None-Match": []string{`"non-matching-etag-value"`}},
			}
			actualRespINMMiss, errINMMiss := c.client.Run(server, &reqIfNoneMatchMiss)
			if errINMMiss != nil {
				st.Fatalf("GET with If-None-Match (miss) failed: %v", errINMMiss)
			}
			testutil.AssertStatusCode(st, testutil.ExpectedResponse{StatusCode: http.StatusOK}, *actualRespINMMiss)
			testutil.AssertBodyEquals(st, []byte(fileContent), actualRespINMMiss.Body)
			st.Logf("If-None-Match (Miss): Status %d OK", actualRespINMMiss.StatusCode)

			// 4. If-Modified-Since: Matching Last-Modified should return 304
			reqIfModSinceHit := testutil.TestRequest{
				Method: "GET", Path: filePathInServer,
				Headers: http.Header{"If-Modified-Since": []string{initialLastModified}},
			}
			actualRespIMSHit, errIMSHit := c.client.Run(server, &reqIfModSinceHit)

			if errIMSHit != nil {
				st.Fatalf("GET with If-Modified-Since (hit) failed: %v", errIMSHit)
			}
			testutil.AssertStatusCode(st, testutil.ExpectedResponse{StatusCode: http.StatusNotModified}, *actualRespIMSHit)
			st.Logf("If-Modified-Since (Hit): Status %d OK", actualRespIMSHit.StatusCode)

			// 5. If-Modified-Since: Older date (file is newer) should return 200
			pastTime := time.Now().Add(-24 * time.Hour).UTC().Format(http.TimeFormat) // A day ago
			reqIfModSinceMiss := testutil.TestRequest{
				Method: "GET", Path: filePathInServer,
				Headers: http.Header{"If-Modified-Since": []string{pastTime}},
			}
			actualRespIMSMiss, errIMSMiss := c.client.Run(server, &reqIfModSinceMiss)
			if errIMSMiss != nil {
				st.Fatalf("GET with If-Modified-Since (miss - file newer) failed: %v", errIMSMiss)
			}
			testutil.AssertStatusCode(st, testutil.ExpectedResponse{StatusCode: http.StatusOK}, *actualRespIMSMiss)
			testutil.AssertBodyEquals(st, []byte(fileContent), actualRespIMSMiss.Body)
			st.Logf("If-Modified-Since (Miss - file newer): Status %d OK", actualRespIMSMiss.StatusCode)

			// 6. Precedence: If-None-Match (match) and If-Modified-Since (indicates modified) -> 304
			//    (If-None-Match takes precedence)
			reqPrecedence1 := testutil.TestRequest{
				Method: "GET", Path: filePathInServer,
				Headers: http.Header{
					"If-None-Match":     []string{initialETag}, // This will match
					"If-Modified-Since": []string{pastTime},    // This alone would yield 200
				},
			}
			actualRespPrec1, errPrec1 := c.client.Run(server, &reqPrecedence1)
			if errPrec1 != nil {
				st.Fatalf("GET with If-None-Match (hit) and If-Modified-Since (miss) failed: %v", errPrec1)
			}
			testutil.AssertStatusCode(st, testutil.ExpectedResponse{StatusCode: http.StatusNotModified}, *actualRespPrec1)
			st.Logf("Precedence Test 1 (INM match, IMS miss): Status %d OK", actualRespPrec1.StatusCode)

			// Scenario 7a: INM (no match), IMS (match) -> 304
			reqPrecedence2a := testutil.TestRequest{
				Method: "GET", Path: filePathInServer,
				Headers: http.Header{
					"If-None-Match":     []string{`"non-matching-etag-value"`}, // This will NOT match
					"If-Modified-Since": []string{initialLastModified},         // This alone would yield 304
				},
			}
			actualRespPrec2a, errPrec2a := c.client.Run(server, &reqPrecedence2a)
			if errPrec2a != nil {
				st.Fatalf("GET with If-None-Match (miss) and If-Modified-Since (hit) failed: %v", errPrec2a)
			}
			testutil.AssertStatusCode(st, testutil.ExpectedResponse{StatusCode: http.StatusNotModified}, *actualRespPrec2a)
			st.Logf("Precedence Test 2a (INM miss, IMS hit): Status %d OK", actualRespPrec2a.StatusCode)

			// Scenario 7b: INM (no match), IMS (no match / file newer) -> 200
			reqPrecedence2b := testutil.TestRequest{
				Method: "GET", Path: filePathInServer,
				Headers: http.Header{
					"If-None-Match":     []string{`"non-matching-etag-value"`}, // This will NOT match
					"If-Modified-Since": []string{pastTime},                    // This alone would yield 200
				},
			}
			actualRespPrec2b, errPrec2b := c.client.Run(server, &reqPrecedence2b)
			if errPrec2b != nil {
				st.Fatalf("GET with If-None-Match (miss) and If-Modified-Since (miss) failed: %v", errPrec2b)
			}
			testutil.AssertStatusCode(st, testutil.ExpectedResponse{StatusCode: http.StatusOK}, *actualRespPrec2b)
			testutil.AssertBodyEquals(st, []byte(fileContent), actualRespPrec2b.Body)
			st.Logf("Precedence Test 2b (INM miss, IMS miss): Status %d OK", actualRespPrec2b.StatusCode)
		})
	}
}

// TestStaticFileServer_MimeTypes is a placeholder.
func TestStaticFileServer_MimeTypes(t *testing.T) {
	t.Parallel()
	t.Skip("TestStaticFileServer_MimeTypes not yet implemented")
}

// TestLogging is a placeholder for logging tests.
func TestLogging(t *testing.T) {
	// TODO: Implement logging tests:
	// - Access log format (JSON) and content.
	// - Error log format and content for different levels.
	// - Log file reopening on SIGHUP (if feasible in E2E).
	// - Real IP logging with TrustedProxies.
	t.Skip("TestLogging not yet implemented")
}

// TestHotReload is a placeholder for hot reload tests.
func TestHotReload(t *testing.T) {
	// TODO: Implement hot reload tests:
	// - Configuration reload without dropping connections.
	// - Binary upgrade (if feasible to simulate in E2E).
	t.Skip("TestHotReload not yet implemented")
}

// TestRouting_MatchingLogic covers spec 1.2.4 (Route Matching Precedence)
// and 1.2.3 (PathPattern types and validation - covered by startup failures in next test).
func TestRouting_MatchingLogic(t *testing.T) {
	t.Parallel()

	if serverBinaryMissing {
		t.Skipf("Skipping E2E test: server binary not found or not executable at '%s'", serverBinaryPath)
	}
	// Setup a simple document root with a few files for handlers to serve.
	// This helps verify that the correct handler (and its config) is invoked.
	docRoot, cleanupDocRoot, err := setupTempFiles(t, map[string]string{
		"exact.txt":         "exact match content",
		"prefix/file1.txt":  "prefix match content 1",
		"prefix/deep/f.txt": "longest prefix match content",
		"root.txt":          "root exact match content",
	})
	if err != nil {
		t.Fatalf("Failed to set up temp files for routing test: %v", err)
	}
	defer cleanupDocRoot()

	baseConfig := config.Config{
		Server: &config.ServerConfig{
			Address: strPtr("127.0.0.1:0"),
		},
		Routing: &config.RoutingConfig{
			Routes: []config.Route{
				// Exact matches
				{
					PathPattern: "/", MatchType: config.MatchTypeExact, HandlerType: "StaticFileServer",
					HandlerConfig: testutil.ToRawMessageWrapper(t, config.StaticFileServerConfig{DocumentRoot: docRoot, IndexFiles: []string{"root.txt"}}),
				},
				{
					PathPattern: "/exact", MatchType: config.MatchTypeExact, HandlerType: "StaticFileServer",
					HandlerConfig: testutil.ToRawMessageWrapper(t, config.StaticFileServerConfig{DocumentRoot: docRoot, IndexFiles: []string{"exact.txt"}}),
				},
				// Prefix matches
				{
					PathPattern: "/prefix/", MatchType: config.MatchTypePrefix, HandlerType: "StaticFileServer", // Serves prefix/
					HandlerConfig: testutil.ToRawMessageWrapper(t, config.StaticFileServerConfig{DocumentRoot: filepath.Join(docRoot, "prefix")}),
				},
				{
					PathPattern: "/prefix/deep/", MatchType: config.MatchTypePrefix, HandlerType: "StaticFileServer", // Serves prefix/deep/
					HandlerConfig: testutil.ToRawMessageWrapper(t, config.StaticFileServerConfig{DocumentRoot: filepath.Join(docRoot, "prefix", "deep")}),
				},
				// Ambiguous route to test precedence: Exact /foo/ vs Prefix /foo/
				{
					PathPattern: "/foo/", MatchType: config.MatchTypeExact, HandlerType: "StaticFileServer", // Exact /foo/ serves this
					HandlerConfig: testutil.ToRawMessageWrapper(t, config.StaticFileServerConfig{DocumentRoot: docRoot, IndexFiles: []string{"exact.txt"}}), // Points to exact.txt for differentiation
				},
				{
					PathPattern: "/foo/", MatchType: config.MatchTypePrefix, HandlerType: "StaticFileServer", // Prefix /foo/ serves this
					HandlerConfig: testutil.ToRawMessageWrapper(t, config.StaticFileServerConfig{DocumentRoot: filepath.Join(docRoot, "prefix")}), // Points to prefix/ for differentiation
				},
			},
		},
		Logging: &config.LoggingConfig{LogLevel: config.LogLevelDebug, AccessLog: &config.AccessLogConfig{Enabled: boolPtr(false)}}, // Disable logs for clarity unless debugging
	}

	testCases := []testutil.E2ETestCase{
		{
			Name:     "GET Root (Exact Match)",
			Request:  testutil.TestRequest{Method: "GET", Path: "/"},
			Expected: testutil.ExpectedResponse{StatusCode: http.StatusOK, BodyMatcher: &testutil.ExactBodyMatcher{ExpectedBody: []byte("root exact match content")}},
		},
		{
			Name:     "GET /exact (Exact Match)",
			Request:  testutil.TestRequest{Method: "GET", Path: "/exact"},
			Expected: testutil.ExpectedResponse{StatusCode: http.StatusOK, BodyMatcher: &testutil.ExactBodyMatcher{ExpectedBody: []byte("exact match content")}},
		},
		{
			Name:     "GET /prefix/file1.txt (Prefix Match)",
			Request:  testutil.TestRequest{Method: "GET", Path: "/prefix/file1.txt"}, // Static server will look for "file1.txt" in its docroot "prefix/"
			Expected: testutil.ExpectedResponse{StatusCode: http.StatusOK, BodyMatcher: &testutil.ExactBodyMatcher{ExpectedBody: []byte("prefix match content 1")}},
		},
		{
			Name:     "GET /prefix/deep/f.txt (Longest Prefix Match)",
			Request:  testutil.TestRequest{Method: "GET", Path: "/prefix/deep/f.txt"},
			Expected: testutil.ExpectedResponse{StatusCode: http.StatusOK, BodyMatcher: &testutil.ExactBodyMatcher{ExpectedBody: []byte("longest prefix match content")}},
		},
		{
			Name:     "GET /foo/ (Exact match should take precedence over Prefix for same path)",
			Request:  testutil.TestRequest{Method: "GET", Path: "/foo/"}, // This requests the "exact.txt" due to Exact /foo/ route
			Expected: testutil.ExpectedResponse{StatusCode: http.StatusOK, BodyMatcher: &testutil.ExactBodyMatcher{ExpectedBody: []byte("exact match content")}},
		},
		{
			Name:     "GET /foo/bar.txt (Prefix /foo/ should match this, as Exact /foo/ does not apply)",
			Request:  testutil.TestRequest{Method: "GET", Path: "/foo/file1.txt"}, // This requests prefix/file1.txt due to Prefix /foo/ route
			Expected: testutil.ExpectedResponse{StatusCode: http.StatusOK, BodyMatcher: &testutil.ExactBodyMatcher{ExpectedBody: []byte("prefix match content 1")}},
		},
		{
			Name:    "GET /no/such/route (No Match)",
			Request: testutil.TestRequest{Method: "GET", Path: "/no/such/route"},
			Expected: testutil.ExpectedResponse{
				StatusCode: http.StatusNotFound,
				Headers:    testutil.HeaderMatcher{"content-type": "application/json; charset=utf-8"}, // Assuming default error response
			},
		},
		// Case sensitivity test: Path patterns are case-sensitive.
		{
			Name:     "GET /EXACT (Case-Sensitive No Match)",
			Request:  testutil.TestRequest{Method: "GET", Path: "/EXACT"},
			Expected: testutil.ExpectedResponse{StatusCode: http.StatusNotFound},
		},
	}

	testDef := testutil.E2ETestDefinition{
		Name:                "RoutingMatchingLogic",
		ServerBinaryPath:    serverBinaryPath,
		ServerConfigData:    baseConfig,
		ServerConfigFormat:  "json",
		ServerConfigArgName: "-config",
		ServerListenAddress: *baseConfig.Server.Address,
		TestCases:           testCases,
		CurlPath:            defaultCurlPath,
	}
	testutil.RunE2ETest(t, testDef)
}

// TestRouting_ConfigValidationFailures tests server startup failures due to invalid routing configurations

// TestHTTPRequestHeaderValidation verifies server behavior for requests with invalid headers
// as per RFC 7540 Section 8.1.2.
func TestHTTPRequestHeaderValidation(t *testing.T) {
	t.Parallel()
	if serverBinaryMissing {
		t.Skipf("Skipping E2E test: server binary not found or not executable at '%s'", serverBinaryPath)
	}

	docRoot, cleanupDocRoot, err := setupTempFiles(t, map[string]string{
		"valid.txt": "This is a valid file.",
	})
	if err != nil {
		t.Fatalf("Failed to set up temp files for header validation test: %v", err)
	}
	defer cleanupDocRoot()

	baseConfig := &config.Config{
		Server: &config.ServerConfig{
			Address: strPtr("127.0.0.1:0"),
		},
		Routing: &config.RoutingConfig{
			Routes: []config.Route{
				{
					PathPattern: "/testpath/",
					MatchType:   config.MatchTypePrefix,
					HandlerType: "StaticFileServer",
					HandlerConfig: testutil.ToRawMessageWrapper(t, config.StaticFileServerConfig{
						DocumentRoot:          docRoot,
						IndexFiles:            []string{"valid.txt"},
						ServeDirectoryListing: boolPtr(false),
					}),
				},
			},
		},
		Logging: &config.LoggingConfig{
			LogLevel:  config.LogLevelDebug,
			AccessLog: &config.AccessLogConfig{Enabled: boolPtr(false)}, // Disable for cleaner test logs
			ErrorLog:  &config.ErrorLogConfig{Target: strPtr("stderr")},
		},
	}

	json400Error := testutil.ExpectedResponse{
		StatusCode: http.StatusBadRequest,
		Headers:    testutil.HeaderMatcher{"content-type": "application/json; charset=utf-8"},
		BodyMatcher: &testutil.JSONFieldsBodyMatcher{
			ExpectedFields: map[string]interface{}{
				"error": map[string]interface{}{
					"status_code": 400.0, // JSON numbers are float64
					"message":     "Bad Request",
				},
			},
		},
	}

	html400Error := testutil.ExpectedResponse{
		StatusCode: http.StatusBadRequest,
		Headers:    testutil.HeaderMatcher{"content-type": "text/html; charset=utf-8"},
		BodyMatcher: &testutil.StringContainsBodyMatcher{
			Substring: "<h1>Bad Request</h1>",
		},
	}

	testCases := []testutil.E2ETestCase{
		// 1. Uppercase header field name
		{
			Name: "Uppercase Header Name - JSON",
			Request: testutil.TestRequest{
				Method:  "GET",
				Path:    "/testpath/valid.txt",
				Headers: http.Header{"X-ALLCAPS-NAME": []string{"value"}, "Accept": []string{"application/json"}},
			},
			Expected: json400Error,
		},
		{
			Name: "Uppercase Header Name - HTML",
			Request: testutil.TestRequest{
				Method:  "GET",
				Path:    "/testpath/valid.txt",
				Headers: http.Header{"X-ALLCAPS-NAME": []string{"value"}, "Accept": []string{"text/html"}},
			},
			Expected: html400Error,
		},

		// 2. Forbidden connection-specific header fields
		// Connection: keep-alive
		{
			Name: "Connection Keep-Alive Header - JSON",
			Request: testutil.TestRequest{
				Method:  "GET",
				Path:    "/testpath/valid.txt",
				Headers: http.Header{"Connection": []string{"keep-alive"}, "Accept": []string{"application/json"}},
			},
			Expected: json400Error,
		},
		{
			Name: "Connection Keep-Alive Header - HTML",
			Request: testutil.TestRequest{
				Method:  "GET",
				Path:    "/testpath/valid.txt",
				Headers: http.Header{"Connection": []string{"keep-alive"}, "Accept": []string{"text/html"}},
			},
			Expected: html400Error,
		},
		// Proxy-Connection: keep-alive
		{
			Name: "Proxy-Connection Keep-Alive Header - JSON",
			Request: testutil.TestRequest{
				Method:  "GET",
				Path:    "/testpath/valid.txt",
				Headers: http.Header{"Proxy-Connection": []string{"keep-alive"}, "Accept": []string{"application/json"}},
			},
			Expected: json400Error,
		},
		{
			Name: "Proxy-Connection Keep-Alive Header - HTML",
			Request: testutil.TestRequest{
				Method:  "GET",
				Path:    "/testpath/valid.txt",
				Headers: http.Header{"Proxy-Connection": []string{"keep-alive"}, "Accept": []string{"text/html"}},
			},
			Expected: html400Error,
		},

		// 3. TE header field with value other than "trailers"
		{
			Name: "TE Compress Header - JSON",
			Request: testutil.TestRequest{
				Method:  "GET",
				Path:    "/testpath/valid.txt",
				Headers: http.Header{"TE": []string{"compress"}, "Accept": []string{"application/json"}},
			},
			Expected: json400Error,
		},
		{
			Name: "TE Compress Header - HTML",
			Request: testutil.TestRequest{
				Method:  "GET",
				Path:    "/testpath/valid.txt",
				Headers: http.Header{"TE": []string{"compress"}, "Accept": []string{"text/html"}},
			},
			Expected: html400Error,
		},
		// TE: compress, gzip (multiple invalid values)
		{
			Name: "TE Compress Gzip Header - JSON",
			Request: testutil.TestRequest{
				Method:  "GET",
				Path:    "/testpath/valid.txt",
				Headers: http.Header{"TE": []string{"compress, gzip"}, "Accept": []string{"application/json"}},
			},
			Expected: json400Error,
		},
		{
			Name: "TE Compress Gzip Header - HTML",
			Request: testutil.TestRequest{
				Method:  "GET",
				Path:    "/testpath/valid.txt",
				Headers: http.Header{"TE": []string{"compress, gzip"}, "Accept": []string{"text/html"}},
			},
			Expected: html400Error,
		},

		// TE: trailers, compress (valid + invalid value)
		// RFC 7540 Section 8.1.2.2: "A request containing the TE header field with any value other than "trailers" MUST be treated as malformed"
		// This implies that if "trailers" is present alongside an invalid value, it's still malformed.
		{
			Name: "TE Trailers Compress Header - JSON",
			Request: testutil.TestRequest{
				Method:  "GET",
				Path:    "/testpath/valid.txt",
				Headers: http.Header{"TE": []string{"trailers, compress"}, "Accept": []string{"application/json"}},
			},
			Expected: json400Error,
		},
		{
			Name: "TE Trailers Compress Header - HTML",
			Request: testutil.TestRequest{
				Method:  "GET",
				Path:    "/testpath/valid.txt",
				Headers: http.Header{"TE": []string{"trailers, compress"}, "Accept": []string{"text/html"}},
			},
			Expected: html400Error,
		},
		{
			Name: "TE Trailers Header (Valid) - Expect 200 OK",
			Request: testutil.TestRequest{
				Method:  "GET",
				Path:    "/testpath/valid.txt",
				Headers: http.Header{"TE": []string{"trailers"}},
			},
			Expected: testutil.ExpectedResponse{
				StatusCode:  http.StatusOK,
				BodyMatcher: &testutil.ExactBodyMatcher{ExpectedBody: []byte("This is a valid file.")},
			},
		},
	}
	testDef := testutil.E2ETestDefinition{
		Name:                "HTTPRequestHeaderValidation",
		ServerBinaryPath:    serverBinaryPath,
		ServerConfigData:    baseConfig,
		ServerConfigFormat:  "json",
		ServerConfigArgName: "-config",
		ServerListenAddress: *baseConfig.Server.Address,
		TestCases:           testCases,
		CurlPath:            defaultCurlPath,
	}
	testutil.RunE2ETest(t, testDef)
}

// as per spec 1.2.3 (PathPattern validation) and 1.2.4 (Ambiguity).
func TestRouting_ConfigValidationFailures(t *testing.T) {
	t.Parallel()

	if serverBinaryMissing {
		t.Skipf("Skipping E2E test: server binary not found or not executable at '%s'", serverBinaryPath)
	}
	tests := []struct {
		name          string
		routes        []config.Route
		expectedError string // Substring expected in server's stderr startup log
	}{
		{
			name: "Exact match ending with / (not root)",
			routes: []config.Route{
				{PathPattern: "/admin/", MatchType: config.MatchTypeExact, HandlerType: "Dummy"},
			},
			expectedError: "configuration error: Exact match PathPattern '/admin/' MUST NOT end with a '/'",
		},
		{
			name: "Prefix match not ending with /",
			routes: []config.Route{
				{PathPattern: "/static", MatchType: config.MatchTypePrefix, HandlerType: "Dummy"},
			},
			expectedError: "configuration error: Prefix match PathPattern '/static' MUST end with a '/'",
		},
		{
			name: "Ambiguous routes (identical PathPattern and MatchType)",
			routes: []config.Route{
				{PathPattern: "/api/users", MatchType: config.MatchTypeExact, HandlerType: "HandlerA"},
				{PathPattern: "/api/users", MatchType: config.MatchTypeExact, HandlerType: "HandlerB"},
			},
			expectedError: "configuration error: Ambiguous route: PathPattern '/api/users' with MatchType 'Exact' is defined more than once",
		},
		// Valid cases that should NOT error, for contrast (not strictly needed for failure test but good for sanity)
		{
			name: "Valid: Exact root",
			routes: []config.Route{
				{PathPattern: "/", MatchType: config.MatchTypeExact, HandlerType: "Dummy"},
			},
			expectedError: "", // No error
		},
		{
			name: "Valid: Prefix root",
			routes: []config.Route{
				{PathPattern: "/", MatchType: config.MatchTypePrefix, HandlerType: "Dummy"},
			},
			expectedError: "", // No error
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.Config{
				Server: &config.ServerConfig{
					Address: strPtr("127.0.0.1:0"),
				},
				Routing: &config.RoutingConfig{
					Routes: tt.routes,
				},
				Logging: &config.LoggingConfig{
					LogLevel: config.LogLevelError, // Keep logs minimal
					ErrorLog: &config.ErrorLogConfig{Target: strPtr("stderr")},
				},
			}

			// For validation failure tests, we expect the server to NOT start up successfully.
			// testutil.StartTestServer will return an error if it can't detect the server listening.
			// We then check the captured server logs for the expected validation error message.

			configFile, cleanup, err := testutil.WriteTempConfig(cfg, "json")
			if err != nil {
				t.Fatalf("Failed to write temp config: %v", err)
			}
			defer cleanup()

			instance, err := testutil.StartTestServer(serverBinaryPath, configFile, "-config", *cfg.Server.Address)

			if tt.expectedError != "" { // Expecting a startup failure
				if err == nil { // Server started unexpectedly
					TeardownServer(instance)
					t.Fatalf("Server started successfully, but expected startup failure with error containing: %s\nServer Logs:\n%s", tt.expectedError, instance.SafeGetLogs())
				}
				// Server failed to start as expected. Now check logs.
				// ServerInstance might be nil if StartTestServer fails very early.
				// testutil.StartTestServer should ideally still return a partially populated instance for log access on failure.
				// For now, we assume log capture happens even if full startup polling fails.
				// This part might need refinement in testutil if instance is nil on startup error.

				var serverLogs string
				if instance != nil {
					serverLogs = instance.SafeGetLogs() // This reads from the buffer
					TeardownServer(instance)            // Attempt cleanup even if startup failed
				} else if startupErr, ok := err.(*exec.ExitError); ok { // If instance is nil, 'err' from StartTestServer might be ExitError containing Stderr
					serverLogs = string(startupErr.Stderr)
				} else if err != nil { // If instance is nil and err is not ExitError, use err.Error()
					serverLogs = err.Error()
				}

				if !strings.Contains(serverLogs, tt.expectedError) {
					t.Errorf("Server startup failed as expected, but logs did not contain the expected error substring.\nExpected: %s\nActual Logs:\n%s", tt.expectedError, serverLogs)
				} else {
					t.Logf("Server startup failed as expected with correct error message: %s", tt.expectedError)
				}

			} else { // Expecting successful startup (valid config)
				if err != nil {
					var serverLogs string
					if instance != nil {
						serverLogs = instance.SafeGetLogs()
					} else if startupErr, ok := err.(*exec.ExitError); ok {
						serverLogs = string(startupErr.Stderr)
					} else if err != nil {
						serverLogs = err.Error()
					}
					t.Fatalf("Server failed to start for a supposedly valid configuration: %v\nServer Logs:\n%s", err, serverLogs)
				}
				TeardownServer(instance) // Cleanly stop the server if it started.
				t.Logf("Server started successfully for valid configuration, as expected.")
			}
		})

	}
}

// END_OF_FILE_MARKER_DO_NOT_USE

var tlsBasicTestDef = testutil.E2ETestDefinition{
	Name:                "TestTLS_Basic",
	ServerBinaryPath:    serverBinaryPath,
	ServerConfigArgName: "-config",
	ServerListenAddress: "127.0.0.1:0", // Dynamic port
	ServerConfigFormat:  "json",
	// ServerConfigData is set by the SetupFunc
	TestCases: []testutil.E2ETestCase{
		{
			Name: "SecureGET_SuccessfulRequest",
			Request: testutil.TestRequest{
				UseTLS: true,
				Method: "GET",
				Path:   "/static/secure.txt",
			},
			Expected: testutil.ExpectedResponse{
				StatusCode:  http.StatusOK,
				BodyMatcher: &testutil.ExactBodyMatcher{ExpectedBody: []byte("secure content")},
			},
		},
		{
			Name: "CleartextH2C_To_TLSPort_SuccessfulRequest",
			Request: testutil.TestRequest{
				UseTLS: false, // This is the key part for H2C test
				Method: "GET",
				Path:   "/static/secure.txt",
			},
			Expected: testutil.ExpectedResponse{
				StatusCode:  http.StatusOK,
				BodyMatcher: &testutil.ExactBodyMatcher{ExpectedBody: []byte("secure content")},
			},
		},
	},
	SetupFunc: func(t *testing.T, def *testutil.E2ETestDefinition) {
		// Generate a self-signed cert and key for the test
		certPath, keyPath, err := testutil.GenerateSelfSignedCertKeyFiles(t, "127.0.0.1")
		if err != nil {
			t.Fatalf("Failed to generate self-signed cert/key: %v", err)
		}

		// Setup a temp file for the handler
		docRoot, cleanupDocRoot, err := setupTempFiles(t, map[string]string{
			"secure.txt": "secure content",
		})
		if err != nil {
			t.Fatalf("Failed to set up temp files for TLS test: %v", err)
		}
		t.Cleanup(cleanupDocRoot)

		// Create a config with TLS enabled
		trueBool := true
		def.ServerConfigData = &config.Config{
			Server: &config.ServerConfig{
				Address: &def.ServerListenAddress,
				TLS: &config.TLSConfig{
					Enabled:  &trueBool,
					CertFile: &certPath,
					KeyFile:  &keyPath,
				},
			},
			Logging: &config.LoggingConfig{
				LogLevel: config.LogLevelDebug,
				AccessLog: &config.AccessLogConfig{
					Enabled: &trueBool,
					Target:  strPtr("stdout"),
				},
			},
			Routing: &config.RoutingConfig{
				Routes: []config.Route{
					{
						PathPattern: "/static/",
						MatchType:   config.MatchTypePrefix,
						HandlerType: "StaticFileServer",
						HandlerConfig: testutil.ToRawMessageWrapper(t, config.StaticFileServerConfig{
							DocumentRoot: docRoot,
						}),
					},
				},
			},
		}
	},
}

func TestTLS_Basic(t *testing.T) {
	if serverBinaryMissing {
		t.Skipf("Skipping E2E test: server binary not found or not executable at '%s'", serverBinaryPath)
	}
	testutil.RunE2ETest(t, tlsBasicTestDef)
}
