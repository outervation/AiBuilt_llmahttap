package server_test

import (
	"bytes"
	"encoding/json"
	"errors" // Standard errors package
	"fmt"

	"context"
	"html"

	"net/http"
	"strconv"
	"strings"
	"testing"

	isserver "example.com/llmahttap/v2/internal/server" // Alias for the package under test
)

// TestPrefersJSON tests the internal PrefersJSON logic from the server package.
func TestPrefersJSON(t *testing.T) {
	tests := []struct {
		name         string
		acceptHeader string
		expected     bool
	}{
		// Rule A: "application/json is the first listed type with q > 0" (or application/*)
		{"A: Exact JSON first", "application/json", true},
		{"A: Exact JSON first, with HTML", "application/json, text/html", true},
		{"A: Exact JSON first, q=0.5", "application/json;q=0.5, text/html", true},
		{"A: application/* first", "application/*, text/html", true},
		{"A: HTML first, JSON second", "text/html, application/json", true}, // True because of Rule C (app/json implies q=1.0)

		// Rule C: "application/json is present with q=1.0"
		{"C: HTML first, JSON second q=1.0", "text/html, application/json;q=1.0", true},
		{"C: HTML first, JSON second default q", "text/html, application/json", true},
		{"C: HTML first, app/* second q=1.0", "text/html, application/*;q=1.0", false}, // application/* is not application/json for Rule C
		{"C: JSON with q=0.9 (not first)", "text/xml, application/json;q=0.9", false},  // Rule A also not met if it's not first and another is first, Rule C needs q=1.0

		// Rule B: "or */* is present and no other type has higher q-value than application/json"
		{"B: */* only", "*/*", true},
		{"B: */*, text/html", "text/html, */*", true}, // html q=1, json_eff_q=1. Not higher.
		{"B: */*, text/html q=1, app/json q=0.5", "text/html;q=1, application/json;q=0.5, */*", true},                                             // html q=1, json_eff_q=1 from */*. Not higher.
		{"B: */* q=0.8, text/html q=0.9", "text/html;q=0.9, */*;q=0.8", false},                                                                    // html q=0.9 > json_eff_q=0.8.
		{"B: */* q=0.9, text/html q=0.8", "text/html;q=0.8, */*;q=0.9", true},                                                                     // html q=0.8 < json_eff_q=0.9.
		{"B: app/json q=0.7, */*;q=0.6, html q=0.8", "application/json;q=0.7, */*;q=0.6, text/html;q=0.8", true},                                  // Rule A: app/json is first with q>0. Expected true.
		{"B: text/xml, html q=0.9, */* q=0.5", "text/xml, application/xhtml+xml, text/html;q=0.9, text/plain;q=0.8, image/png, */*;q=0.5", false}, // html q=0.9 > json_eff_q=0.5

		// Combinations and Edge Cases
		{"Empty Accept", "", false},
		{"Only HTML", "text/html", false},
		{"HTML and others", "text/html, application/xhtml+xml, application/xml;q=0.9, image/webp, */*;q=0.8", false},
		{"JSON q=0", "application/json;q=0", false},
		{"HTML, JSON q=0", "text/html, application/json;q=0", false},
		{"Malformed q", "application/json;q=foo", false}, // parseQValue returns 0 for malformed, effectively q=0
		{"Multiple JSON, first wins (Rule A)", "application/json;q=0.8, application/json;q=0.9", true},
		{"Mixed, JSON preferred by A", "application/json, text/plain;q=0.9, text/html;q=0.8", true},
		{"Mixed, JSON preferred by C", "text/plain;q=0.9, application/json;q=1.0, text/html;q=0.8", true},
		{"Mixed, JSON preferred by B", "text/plain;q=0.7, */*;q=0.8, text/html;q=0.6", true},    // plain q=0.7 < json_eff_q=0.8
		{"Mixed, HTML preferred over B", "text/plain;q=0.9, */*;q=0.8, text/html;q=0.6", false}, // plain q=0.9 > json_eff_q=0.8

		// Specific scenarios from spec 5.2.1
		{"Spec A1: application/json", "application/json", true},
		{"Spec A2: application/json, text/javascript, */*", "application/json, text/javascript, */*", true},
		{"Spec B1: */*", "*/*", true},
		{"Spec B2: text/html, */*", "text/html, */*", true},                           // html default q=1, */* default q=1. Effective JSON q=1. html not higher.
		{"Spec B3: text/html; q=0.8, */*", "text/html; q=0.8, */*", true},             // html q=0.8, */* default q=1. Effective JSON q=1. html not higher.
		{"Spec C1: text/html, application/json", "text/html, application/json", true}, // application/json default q=1.0
		{"Spec C2: text/html, application/json;q=1.0", "text/html, application/json;q=1.0", true},

		// Test cases for application/* behavior (treated like application/json for preference strength)
		{"A: app/* first", "application/*", true},
		{"A: app/* first, with html", "application/*, text/html", true},
		{"A: text/html, app/*", "text/html, application/*", false},                                                                                    // Fails A for app/*. If app/* is not q=1, also fails C. Might pass B if */* involved.
		{"C: text/html, app/* q=1", "text/html, application/*;q=1.0", false},                                                                          // Rule C is specific to "application/json", not "application/*"
		{"B: app/* q=0.7, */*;q=0.6, html q=0.8", "application/*;q=0.7, */*;q=0.6, text/html;q=0.8", true},                                            // Rule A: app/* is first with q>0. Expected true.
		{"B: text/plain q=0.7, */*;q=0.8, app/*;q=0.75, text/html;q=0.6", "text/plain;q=0.7, */*;q=0.8, application/*;q=0.75, text/html;q=0.6", true}, // plain q=0.7 < json_eff_q=0.8 (from */*)

		// Checking interaction of rules: A, B, C
		{"Actual A: app/json;q=0.1, text/html", "application/json;q=0.1, text/html", true},                                  // Rule A.
		{"Actual A: app/*;q=0.1, text/html", "application/*;q=0.1, text/html", true},                                        // Rule A.
		{"Actual A fails, C passes: text/html, app/json;q=1.0", "text/html, application/json;q=1.0", true},                  // Rule C.
		{"Actual A fails, C passes: text/html, app/json", "text/html, application/json", true},                              // Rule C (default q=1.0).
		{"Actual A fails, C fails, B passes: text/html;q=0.8, */*;q=0.9", "text/html;q=0.8, */*;q=0.9", true},               // Rule B (html 0.8 < effective json 0.9 from */*)
		{"Actual B: text/html, */*;q=0.9", "text/html, */*;q=0.9", false},                                                   // Rule B (html 1.0 > effective json 0.9 from */*)
		{"text/html;q=0.7, application/json;q=0.5, */*;q=0.6", "text/html;q=0.7, application/json;q=0.5, */*;q=0.6", false}, // text/html (0.7) is higher than application/json (0.5) and */* (0.6)
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isserver.PrefersJSON(tt.acceptHeader); got != tt.expected {
				t.Errorf("isserver.PrefersJSON(%q) = %v, want %v", tt.acceptHeader, got, tt.expected)
			}
		})
	}
}

// mockResponseWriter is a test utility implementing the http2.ResponseWriter interface.
// In a real scenario, this would be `example.com/llmahttap/v2/internal/http2.ResponseWriter`.
// For this test, we define it locally to match the expected signature for WriteErrorResponse.
type mockResponseWriter struct {
	t *testing.T

	headersSent   bool
	sentHeaders   []isserver.HeaderField
	sentData      bytes.Buffer
	endStreamData bool // Was endStream true on the WriteData call
	endStreamHead bool // Was endStream true on the SendHeaders call

	// For simulating errors
	sendHeadersErr error
	writeDataErr   error
}

func newMockResponseWriter(t *testing.T) *mockResponseWriter {
	return &mockResponseWriter{t: t}
}

func (m *mockResponseWriter) SendHeaders(headers []isserver.HeaderField, endStream bool) error {
	if m.sendHeadersErr != nil {
		return m.sendHeadersErr
	}
	if m.headersSent {
		m.t.Helper()
		m.t.Fatal("SendHeaders called more than once")
	}
	m.headersSent = true
	m.sentHeaders = make([]isserver.HeaderField, len(headers)) // Changed type here
	copy(m.sentHeaders, headers)
	m.endStreamHead = endStream
	return nil
}

func (m *mockResponseWriter) WriteData(p []byte, endStream bool) (n int, err error) {
	if m.writeDataErr != nil {
		return 0, m.writeDataErr
	}
	if !m.headersSent {
		m.t.Helper()
		m.t.Fatal("WriteData called before SendHeaders")
	}
	if m.endStreamData {
		m.t.Helper()
		m.t.Fatal("WriteData called after stream already ended by WriteData")
	}
	if m.endStreamHead && len(p) > 0 {
		m.t.Helper()
		m.t.Fatal("WriteData called with data after SendHeaders with endStream=true")
	}
	// Allow WriteData(nil, true) or WriteData([]byte{}, true) even if endStreamHead was true
	if m.endStreamHead && endStream && len(p) == 0 {
		// This is acceptable.
	}

	m.sentData.Write(p)
	m.endStreamData = endStream
	return len(p), nil
}

func (m *mockResponseWriter) WriteTrailers(trailers []isserver.HeaderField) error {
	m.t.Helper()
	m.t.Fatal("WriteTrailers should not be called by WriteErrorResponse")
	return nil
}

func findHeaderValue(headers []isserver.HeaderField, name string) (string, bool) {
	for _, h := range headers {
		if h.Name == name {
			return h.Value, true
		}
	}
	return "", false
}

func TestWriteErrorResponse(t *testing.T) {
	// Set up the mock for json.Marshal using the exported helper from the server package
	originalJSONMarshal := isserver.TestingOnlySetJSONMarshal(json.Marshal)
	defer isserver.TestingOnlySetJSONMarshal(originalJSONMarshal) // Restore original

	tests := []struct {
		name                 string
		acceptHeader         string
		statusCode           int
		detailMessage        string
		mockJsonMarshalError bool
		mockSendHeadersError error
		mockWriteDataError   error
		expectOverallError   bool
		expectedStatus       string
		expectedContentType  string
		bodyChecker          func(t *testing.T, body []byte, statusCode int, detail string)
	}{
		{
			name:                "JSON response for 404",
			acceptHeader:        "application/json",
			statusCode:          http.StatusNotFound,
			detailMessage:       "Custom not found detail",
			expectedStatus:      "404",
			expectedContentType: "application/json; charset=utf-8",
			bodyChecker: func(t *testing.T, body []byte, statusCode int, detail string) {
				var resp isserver.ErrorResponseJSON // Use the type from the server package
				if err := json.Unmarshal(body, &resp); err != nil {
					t.Fatalf("Failed to unmarshal JSON body: %v. Body: %s", err, string(body))
				}
				if resp.Error.StatusCode != statusCode {
					t.Errorf("Expected status code %d in JSON, got %d", statusCode, resp.Error.StatusCode)
				}
				expectedMsg := http.StatusText(statusCode)
				if resp.Error.Message != expectedMsg {
					t.Errorf("Expected message %q in JSON, got %q", expectedMsg, resp.Error.Message)
				}
				if resp.Error.Detail != detail {
					t.Errorf("Expected detail %q in JSON, got %q", detail, resp.Error.Detail)
				}
			},
		},
		{
			name:                "HTML response for 500 (default)",
			acceptHeader:        "text/html",
			statusCode:          http.StatusInternalServerError,
			detailMessage:       "DB error",
			expectedStatus:      "500",
			expectedContentType: "text/html; charset=utf-8",
			bodyChecker: func(t *testing.T, body []byte, statusCode int, detail string) {
				bodyStr := string(body)
				// Use the exported helper from the server package
				defaultMsgInfo, _ := isserver.GetDefaultHTMLMessageInfo(statusCode)
				if !strings.Contains(bodyStr, html.EscapeString(defaultMsgInfo.Title)) {
					t.Errorf("HTML body does not contain title %q. Body: %s", defaultMsgInfo.Title, bodyStr)
				}
				if !strings.Contains(bodyStr, html.EscapeString(defaultMsgInfo.Heading)) {
					t.Errorf("HTML body does not contain heading %q. Body: %s", defaultMsgInfo.Heading, bodyStr)
				}
				expectedContent := defaultMsgInfo.Message
				if detail != "" {
					expectedContent += " " + html.EscapeString(detail)
				}
				if !strings.Contains(bodyStr, expectedContent) {
					t.Errorf("HTML body does not contain message part %q. Expected: %q. Body: %s", expectedContent, expectedContent, bodyStr)
				}
			},
		},
		{
			name:                "HTML response for unknown status 599",
			acceptHeader:        "", // Default to HTML
			statusCode:          599,
			detailMessage:       "Very weird error",
			expectedStatus:      "599",
			expectedContentType: "text/html; charset=utf-8",
			bodyChecker: func(t *testing.T, body []byte, statusCode int, detail string) {
				bodyStr := string(body)
				statusText := http.StatusText(statusCode)
				if statusText == "" {
					statusText = "Error"
				}
				expectedTitle := fmt.Sprintf("%d %s", statusCode, statusText)
				if !strings.Contains(bodyStr, html.EscapeString(expectedTitle)) {
					t.Errorf("HTML body does not contain title %q. Body: %s", expectedTitle, bodyStr)
				}
				if !strings.Contains(bodyStr, html.EscapeString(statusText)) { // Heading
					t.Errorf("HTML body does not contain heading %q. Body: %s", statusText, bodyStr)
				}
				expectedPMessage := html.EscapeString(detail)
				if detail == "" { // As per errors.go logic for unknown codes
					expectedPMessage = "The server encountered an error."
				}
				if !strings.Contains(bodyStr, fmt.Sprintf("<p>%s</p>", expectedPMessage)) {
					t.Errorf("HTML body does not contain detail message paragraph %q. Body: %s", fmt.Sprintf("<p>%s</p>", expectedPMessage), bodyStr)
				}
			},
		},
		{
			name:                 "JSON marshal error fallback to HTML for 403",
			acceptHeader:         "application/json", // Client wants JSON
			statusCode:           http.StatusForbidden,
			detailMessage:        "Token expired",
			mockJsonMarshalError: true,
			expectedStatus:       "403",
			expectedContentType:  "text/html; charset=utf-8", // Fallback
			bodyChecker: func(t *testing.T, body []byte, statusCode int, detail string) {
				bodyStr := string(body)
				defaultMsgInfo, _ := isserver.GetDefaultHTMLMessageInfo(statusCode)
				if !strings.Contains(bodyStr, html.EscapeString(defaultMsgInfo.Title)) {
					t.Errorf("HTML body (fallback) does not contain title %q. Body: %s", defaultMsgInfo.Title, bodyStr)
				}
				expectedContent := defaultMsgInfo.Message
				if detail != "" {
					expectedContent += " " + html.EscapeString(detail)
				}
				if !strings.Contains(bodyStr, expectedContent) {
					t.Errorf("HTML body (fallback) does not contain message part %q. Body: %s", expectedContent, bodyStr)
				}
			},
		},
		{
			name:                "HTML response no detail for 404",
			acceptHeader:        "text/html",
			statusCode:          http.StatusNotFound,
			detailMessage:       "", // No detail
			expectedStatus:      "404",
			expectedContentType: "text/html; charset=utf-8",
			bodyChecker: func(t *testing.T, body []byte, statusCode int, detail string) {
				bodyStr := string(body)
				defaultMsgInfo, _ := isserver.GetDefaultHTMLMessageInfo(statusCode)
				if !strings.Contains(bodyStr, html.EscapeString(defaultMsgInfo.Title)) {
					t.Errorf("HTML body does not contain title %q. Body: %s", defaultMsgInfo.Title, bodyStr)
				}
				expectedPContent := fmt.Sprintf("<p>%s</p>", defaultMsgInfo.Message)
				if !strings.Contains(bodyStr, expectedPContent) {
					t.Errorf("HTML body does not contain exact base message paragraph %q. Body: %s", expectedPContent, bodyStr)
				}
			},
		},
		{
			name:                "JSON response no detail for 400 (omitempty check)",
			acceptHeader:        "application/json, */*;q=0.8",
			statusCode:          http.StatusBadRequest,
			detailMessage:       "", // No detail, so "detail" field should be omitted in JSON
			expectedStatus:      "400",
			expectedContentType: "application/json; charset=utf-8",
			bodyChecker: func(t *testing.T, body []byte, statusCode int, detail string) {
				var resp isserver.ErrorResponseJSON
				if err := json.Unmarshal(body, &resp); err != nil {
					t.Fatalf("Failed to unmarshal JSON body: %v. Body: %s", err, string(body))
				}
				if resp.Error.StatusCode != statusCode {
					t.Errorf("Expected status code %d in JSON, got %d", statusCode, resp.Error.StatusCode)
				}
				if resp.Error.Detail != "" {
					t.Errorf("Expected empty detail in JSON struct, got %q. Body: %s", resp.Error.Detail, string(body))
				}
				var rawMap map[string]interface{}
				if err := json.Unmarshal(body, &rawMap); err != nil {
					t.Fatalf("Failed to unmarshal raw JSON: %v. Body: %s", err, string(body))
				}
				errorMap, ok := rawMap["error"].(map[string]interface{})
				if !ok {
					t.Fatalf("'error' key not found or not a map in JSON. Body: %s", string(body))
				}
				if _, detailKeyExists := errorMap["detail"]; detailKeyExists {
					t.Errorf("'detail' key should be omitted from JSON when detail message is empty. Body: %s", string(body))
				}
			},
		},
		{
			name:                 "SendHeaders error",
			acceptHeader:         "application/json",
			statusCode:           http.StatusServiceUnavailable,
			mockSendHeadersError: errors.New("mock send headers error"),
			expectOverallError:   true,
		},
		{
			name:                "WriteData error",
			acceptHeader:        "text/plain", // Will default to HTML
			statusCode:          http.StatusBadGateway,
			detailMessage:       "upstream failed",
			mockWriteDataError:  errors.New("mock write data error"),
			expectOverallError:  true,
			expectedStatus:      "502", // Headers should still be checked
			expectedContentType: "text/html; charset=utf-8",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.mockJsonMarshalError {
				isserver.TestingOnlySetJSONMarshal(func(v interface{}) ([]byte, error) {
					return nil, errors.New("mock json marshal error")
				})
			} else {
				isserver.TestingOnlySetJSONMarshal(json.Marshal) // Reset to actual marshal
			}

			mockRW := newMockResponseWriter(t)
			mockRW.sendHeadersErr = tt.mockSendHeadersError
			mockRW.writeDataErr = tt.mockWriteDataError

			reqHeaders := []isserver.HeaderField{}
			if tt.acceptHeader != "" {
				reqHeaders = append(reqHeaders, isserver.HeaderField{Name: "accept", Value: tt.acceptHeader})
			}

			err := isserver.WriteErrorResponse(mockRW, tt.statusCode, reqHeaders, tt.detailMessage)

			if tt.expectOverallError {
				if err == nil {
					t.Fatal("isserver.WriteErrorResponse expected an error, but got nil")
				}
				if tt.mockSendHeadersError != nil && err != nil {
					if !strings.Contains(err.Error(), tt.mockSendHeadersError.Error()) {
						t.Errorf("Expected error to contain mock send headers error %q, got %q", tt.mockSendHeadersError.Error(), err.Error())
					}
				}
				// If WriteData was mocked to error, check that the error is reported
				if tt.mockWriteDataError != nil && err != nil {
					if !strings.Contains(err.Error(), tt.mockWriteDataError.Error()) {
						t.Errorf("Expected error to contain mock write data error %q, got %q", tt.mockWriteDataError.Error(), err.Error())
					}
				}
			} else {
				if err != nil {
					t.Fatalf("isserver.WriteErrorResponse returned an unexpected error: %v", err)
				}
			}

			// Header checks should happen if SendHeaders wasn't mocked to fail.
			// If SendHeaders failed, subsequent checks might be on uninitialized/partially sent headers.
			if tt.mockSendHeadersError == nil {
				if !mockRW.headersSent {
					// This should only be hit if we didn't expect an error from SendHeaders,
					// but it wasn't called (which would be an issue in WriteErrorResponse).
					t.Fatal("SendHeaders was not called but was expected")
				} else { // Headers were sent (or attempted successfully)
					statusVal, ok := findHeaderValue(mockRW.sentHeaders, ":status")
					if !ok {
						t.Error("':status' header not found")
					} else if statusVal != tt.expectedStatus {
						t.Errorf("Expected :status %q, got %q", tt.expectedStatus, statusVal)
					}

					contentTypeVal, ok := findHeaderValue(mockRW.sentHeaders, "content-type")
					if !ok {
						t.Error("'content-type' header not found")
					} else if contentTypeVal != tt.expectedContentType {
						t.Errorf("Expected content-type %q, got %q", tt.expectedContentType, contentTypeVal)
					}

					contentLengthStr, ok := findHeaderValue(mockRW.sentHeaders, "content-length")
					if !ok {
						t.Error("'content-length' header not found")
					} else {
						actualHeaderLen, _ := strconv.Atoi(contentLengthStr)
						expectedLen := 0

						if tt.mockWriteDataError != nil && !tt.mockJsonMarshalError {
							// If WriteData errors (and JSON marshal didn't, ensuring HTML path for WriteData error test),
							// content-length reflects the body that *would* have been sent.
							// Calculate the expected length of the HTML body.
							statusText := http.StatusText(tt.statusCode)
							if statusText == "" {
								statusText = "Error"
							}

							var finalTitle, finalHeading, baseMessage string
							defaultMsgInfo, isKnownCode := isserver.GetDefaultHTMLMessageInfo(tt.statusCode)

							if isKnownCode {
								finalTitle = defaultMsgInfo.Title
								finalHeading = defaultMsgInfo.Heading
								baseMessage = defaultMsgInfo.Message
							} else {
								finalTitle = fmt.Sprintf("%d %s", tt.statusCode, statusText)
								finalHeading = statusText
								baseMessage = "The server encountered an error."
							}

							htmlSafeMessageBody := baseMessage
							if tt.detailMessage != "" {
								escapedDetail := html.EscapeString(tt.detailMessage)
								if !isKnownCode {
									htmlSafeMessageBody = escapedDetail
								} else {
									htmlSafeMessageBody = baseMessage + " " + escapedDetail
								}
							}
							// Reconstruct body exactly as WriteErrorResponse would
							bodyBytes := isserver.GenerateHTMLResponseBodyForTest(finalTitle, finalHeading, htmlSafeMessageBody)
							expectedLen = len(bodyBytes)
						} else if tt.mockJsonMarshalError {
							// If JSON marshal errors, it falls back to HTML. Calculate HTML length.
							statusText := http.StatusText(tt.statusCode)
							if statusText == "" {
								statusText = "Error"
							}
							var finalTitle, finalHeading, baseMessage string
							defaultMsgInfo, isKnownCode := isserver.GetDefaultHTMLMessageInfo(tt.statusCode)
							if isKnownCode {
								finalTitle = defaultMsgInfo.Title
								finalHeading = defaultMsgInfo.Heading
								baseMessage = defaultMsgInfo.Message
							} else {
								finalTitle = fmt.Sprintf("%d %s", tt.statusCode, statusText)
								finalHeading = statusText
								baseMessage = "The server encountered an error."
							}
							htmlSafeMessageBody := baseMessage
							if tt.detailMessage != "" {
								escapedDetail := html.EscapeString(tt.detailMessage)
								if !isKnownCode {
									htmlSafeMessageBody = escapedDetail
								} else {
									htmlSafeMessageBody = baseMessage + " " + escapedDetail
								}
							}
							bodyBytes := isserver.GenerateHTMLResponseBodyForTest(finalTitle, finalHeading, htmlSafeMessageBody)
							expectedLen = len(bodyBytes)
						} else {
							// Normal case (no WriteData error, no JSON marshal error): content-length matches actual data written by mock.
							expectedLen = mockRW.sentData.Len()
						}

						if actualHeaderLen != expectedLen {
							t.Errorf("Expected content-length %d, got %s (%d). Body data len: %d", expectedLen, contentLengthStr, actualHeaderLen, mockRW.sentData.Len())
						}
					}

					expectedCacheHeaders := map[string]string{
						"cache-control": "no-cache, no-store, must-revalidate",
						"pragma":        "no-cache",
						"expires":       "0",
					}
					for name, expectedVal := range expectedCacheHeaders {
						val, ok := findHeaderValue(mockRW.sentHeaders, name)
						if !ok {
							t.Errorf("Expected cache header %q not found", name)
						} else if val != expectedVal {
							t.Errorf("Expected cache header %s: %q, got %q", name, expectedVal, val)
						}
					}

					// Body check only if WriteData was not mocked to fail and overall error not expected from SendHeaders
					if tt.bodyChecker != nil && tt.mockWriteDataError == nil {
						tt.bodyChecker(t, mockRW.sentData.Bytes(), tt.statusCode, tt.detailMessage)
					}

					// Check endStream flags
					bodyWasIntended := false
					clHeader, clOk := findHeaderValue(mockRW.sentHeaders, "content-length")
					if clOk {
						clVal, _ := strconv.Atoi(clHeader)
						if clVal > 0 {
							bodyWasIntended = true
						}
					}

					if !bodyWasIntended { // No body was intended (e.g. content-length: 0)
						if !mockRW.endStreamHead {
							t.Error("Expected SendHeaders endStream=true (no body intended or empty body), got false")
						}
						// If WriteData was called with (nil, true) or ({}, true), endStreamData would be true.
						// If WriteData wasn't called (because body was nil/empty), endStreamData is false.
						// This is fine as SendHeaders already ended the stream.
					} else { // Body was intended
						if mockRW.endStreamHead {
							t.Error("Expected SendHeaders endStream=false (body intended), got true")
						}
						// Only check endStreamData if WriteData was not mocked to fail AND a body was successfully written by the mock.
						if tt.mockWriteDataError == nil && mockRW.sentData.Len() > 0 && !mockRW.endStreamData {
							t.Error("Expected WriteData endStream=true for non-empty body (and no WriteData error), got false")
						}
					}
				}
			}
		})
	}
}

func (m *mockResponseWriter) Context() context.Context {
	return context.Background() // Basic implementation for mock
}

func (m *mockResponseWriter) ID() uint32 {
	// For this mock, a static ID is fine, or it could be configurable if needed for specific tests.
	return 1 // Example stream ID
}
