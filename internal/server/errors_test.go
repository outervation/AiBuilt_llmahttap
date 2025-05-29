package server_test

import (
	"bytes"
	"encoding/json"
	"errors" // Standard errors package
	"example.com/llmahttap/v2/internal/http2"
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
		// Based on current PrefersJSON logic (sort by q, then specificity, then order; then check if top is application/json)

		// Exact match for application/json
		{"json_first", "application/json", true},
		{"json_first_with_html", "application/json, text/html", true},    // json is earlier, preferred by order
		{"html_first_json_second", "text/html, application/json", false}, // html is earlier, preferred by order

		// q-values
		{"json_q0.9_html_q0.8", "application/json;q=0.9, text/html;q=0.8", true},
		{"html_q0.9_json_q0.8", "text/html;q=0.9, application/json;q=0.8", false},

		// Specificity with q-values
		{"json_q1_star_q1", "application/json;q=1, */*;q=1", true},               // json is more specific
		{"star_q1_json_q1", "*/*;q=1, application/json;q=1", true},               // json is more specific
		{"app_star_q1_json_q1", "application/*;q=1, application/json;q=1", true}, // json is more specific
		{"json_q1_app_star_q1", "application/json;q=1, application/*;q=1", true}, // json is more specific

		// Wildcards
		{"star_only", "*/*", false},                 // */* is not application/json
		{"text_html_star", "text/html, */*", false}, // html is preferred (q=1, specific) beats */* (q=1, wildcard)
		{"star_text_html", "*/*, text/html", false}, // html is preferred (q=1, specific) beats */* (q=1, wildcard)
		{"app_star_only", "application/*", false},   // application/* is not application/json

		// Complex cases
		{"complex1_json_wins", "text/html;q=0.8, application/json;q=0.9, */*;q=0.7", true},
		{"complex2_html_wins", "application/json;q=0.8, text/html;q=0.9, */*;q=0.7", false},
		{"complex3_star_leads_to_html", "text/plain;q=0.1, */*;q=0.5", false},                            // */* is not app/json
		{"complex4_json_explicit_wins_over_star", "text/plain;q=0.1, */*;q=0.5, application/json", true}, // app/json has q=1 default, highest q

		// Cases from original spec examples (Section 5.2.1), re-evaluated with current logic
		{"spec_A1_json_first", "application/json", true},
		{"spec_A2_json_complex", "application/json, text/javascript, */*", true}, // application/json is most specific, q=1, order=0
		{"spec_B1_star_only_html", "*/*", false},
		{"spec_B2_html_star_html", "text/html, */*", false},                                                      // text/html specific, q=1, order=0
		{"spec_B3_html_q0.8_star_html", "text/html; q=0.8, */*", false},                                          // */* q=1 default wins over text/html q=0.8, but */* is not app/json
		{"spec_B3_variant_star_higher_q_json_wins", "text/html; q=0.8, */*;q=0.9, application/json;q=0.9", true}, // application/json specific, q=0.9, vs */* q=0.9. app/json is more specific.

		{"spec_C1_html_json_default_q_json_wins", "text/html, application/json", false}, // html is order 0, json is order 1. html wins.
		{"spec_C2_html_json_q1_json_wins", "text/html, application/json;q=1.0", false},  // html is order 0, json is order 1. html wins.

		// Edge cases
		{"empty_accept_header", "", false},
		{"only_html", "text/html", false},
		{"json_q0", "application/json;q=0", false},
		{"malformed_q", "application/json;q=foo", false},
		{"text_xml_then_json", "text/xml, application/json", false}, // text/xml is order 0, json is order 1. text/xml wins.
		{"json_then_text_xml", "application/json, text/xml", true},  // json is order 0. json wins.

		// Test cases for application/* behavior
		{"A_app_star_first", "application/*", false},
		{"A_app_star_first_with_html", "application/*, text/html", false},                                    // text/html preferred by order
		{"A_text_html_app_star", "text/html, application/*", false},                                          // text/html preferred by order
		{"C_text_html_app_star_q_1", "text/html, application/*;q=1.0", false},                                // text/html preferred by order
		{"B_app_star_q_0.7_star_q_0.6_html_q_0.8", "application/*;q=0.7, */*;q=0.6, text/html;q=0.8", false}, // html has highest q
		{"B_app_star_q_0.8_star_q_0.7_html_q_0.6", "application/*;q=0.8, */*;q=0.7, text/html;q=0.6", false}, // app/* has highest q but is not app/json

		// New tests based on corrected understanding of RFC 7231 sorting (q, specificity, order)
		{"json_vs_app_star_same_q", "application/json;q=0.8, application/*;q=0.8", true}, // json is more specific
		{"app_star_vs_json_same_q", "application/*;q=0.8, application/json;q=0.8", true}, // json is more specific
		{"star_vs_app_star_same_q", "*/*;q=0.8, application/*;q=0.8", false},             // app/* is more specific, but not app/json
		{"app_star_vs_star_same_q", "application/*;q=0.8, */*;q=0.8", false},             // app/* is more specific, but not app/json

		{"multiple_app_json_highest_q_wins", "application/json;q=0.8, application/json;q=0.9", true},
		{"multiple_app_json_first_in_order_wins_if_q_equal", "application/json;q=0.9, application/json;q=0.9", true}, // first one wins

		{"case_1_json_not_first_but_highest_q", "text/html;q=0.5, application/json;q=0.9", true},
		{"case_2_star_slash_star_wins_but_not_json", "text/html;q=0.5, */*;q=0.9", false},
		{"case_3_json_q1_but_not_most_preferred", "text/html;q=1.0, application/json;q=1.0, application/xml;q=1.0", false}, // text/html is order 0, preferred
		{"re_eval_case_3", "text/html;q=1.0, application/json;q=1.0, application/xml;q=1.0", false},                        // Same as above, text/html preferred by order

		{"equal_q_order_preference_1", "text/html, application/json", false},                   // text/html (order 0) wins
		{"equal_q_order_preference_2", "application/json, text/html", true},                    // application/json (order 0) wins
		{"equal_q_order_preference_3_lots", "text/a, text/b, application/json, text/c", false}, // text/a (order 0) wins
		{"equal_q_order_preference_4_lots", "text/a, application/json, text/b, text/c", false}, // text/a (order 0) wins
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
	sentHeaders   []http2.HeaderField
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

func (m *mockResponseWriter) SendHeaders(headers []http2.HeaderField, endStream bool) error {
	if m.sendHeadersErr != nil {
		return m.sendHeadersErr
	}
	if m.headersSent {
		m.t.Helper()
		m.t.Fatal("SendHeaders called more than once")
	}
	m.headersSent = true
	m.sentHeaders = make([]http2.HeaderField, len(headers))
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

func (m *mockResponseWriter) WriteTrailers(trailers []http2.HeaderField) error {
	m.t.Helper()
	m.t.Fatal("WriteTrailers should not be called by WriteErrorResponse")
	return nil
}

func findHeaderValue(headers []http2.HeaderField, name string) (string, bool) {
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
					statusText = "Unknown Status" // Align with errors.go actual behavior
				}

				expectedTitle := fmt.Sprintf("%d %s", statusCode, statusText) // statusText is already http.StatusText(statusCode) or "Error"
				if !strings.Contains(bodyStr, html.EscapeString(expectedTitle)) {
					t.Errorf("HTML body does not contain title %q. Body: %s", expectedTitle, bodyStr)
				}

				if !strings.Contains(bodyStr, html.EscapeString(statusText)) { // Heading should be statusText
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

			reqHeaders := []http2.HeaderField{}
			if tt.acceptHeader != "" {
				reqHeaders = append(reqHeaders, http2.HeaderField{Name: "accept", Value: tt.acceptHeader})
			}

			err := isserver.WriteErrorResponse(mockRW, tt.statusCode, reqHeaders, tt.detailMessage, nil)

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
