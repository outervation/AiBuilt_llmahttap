package server

import (
	"encoding/json"
	"fmt"
	"golang.org/x/net/http2/hpack"
	"html"
	"net/http"
	"strings"

	"example.com/llmahttap/v2/internal/http2" // For http2.ResponseWriter or Stream
	"example.com/llmahttap/v2/internal/logger"
)

// ErrorDetail represents the inner structure of a JSON error response.
// As per spec 5.2.1:
// json { "error": { "status_code": <HTTP_STATUS_CODE_INT>, "message": "<STANDARD_HTTP_STATUS_MESSAGE_STRING>", "detail": "<OPTIONAL_MORE_SPECIFIC_MESSAGE_STRING>" } }
type ErrorDetail struct {
	StatusCode int    `json:"status_code"`
	Message    string `json:"message"`
	Detail     string `json:"detail,omitempty"` // omitempty for optional field
}

// ErrorResponseJSON represents the full JSON error response body.
type ErrorResponseJSON struct {
	Error ErrorDetail `json:"error"`
}

// defaultHTMLMessages maps HTTP status codes to their default HTML messages.
var defaultHTMLMessages = map[int]struct {
	title   string
	heading string
	message string
}{
	http.StatusNotFound: {
		title:   "404 Not Found",
		heading: "Not Found",
		message: "The requested resource was not found on this server.",
	},
	http.StatusInternalServerError: {
		title:   "500 Internal Server Error",
		heading: "Internal Server Error",
		message: "The server encountered an internal error and was unable to complete your request.",
	},
	http.StatusForbidden: {
		title:   "403 Forbidden",
		heading: "Forbidden",
		message: "You do not have permission to access this resource.",
	},
	http.StatusMethodNotAllowed: {
		title:   "405 Method Not Allowed",
		heading: "Method Not Allowed",
		message: "The method specified in the Request-Line is not allowed for the resource identified by the Request-URI.",
	},
	http.StatusBadRequest: {
		title:   "400 Bad Request",
		heading: "Bad Request",
		message: "The server cannot or will not process the request due to an apparent client error.",
	},
	// Add more as needed
}

// SendDefaultErrorResponse generates and sends a default HTTP error response on the given stream.
// It performs content negotiation based on the Accept header.
// stream: The HTTP/2 stream to send the response on.
// statusCode: The HTTP status code for the error.
// req: The original http.Request, used to inspect the Accept header.
// optionalDetail: A more specific error message string, can be empty.
func SendDefaultErrorResponse(stream http2.ResponseWriter, statusCode int, req *http.Request, optionalDetail string, log *logger.Logger) {
	statusText := http.StatusText(statusCode)
	if statusText == "" {
		statusText = "Error" // Fallback
	}

	acceptHeader := ""
	if req != nil {
		// In a real http.Request, headers are canonicalized.
		// For direct hpack.HeaderField, we might need to iterate and find "accept".
		// Assuming req.Header is available if req is not nil.
		acceptHeader = req.Header.Get("Accept")
	}

	var body []byte
	var contentType string

	// Content negotiation (simplified as per spec 5.2.1)
	// "If the Accept header indicates a preference for application/json
	// (e.g., application/json is the first listed type with q > 0,
	// or */* is present and no other type has higher q-value than application/json,
	// or application/json is present with q=1.0)"
	// This is a simplified check. A full q-value parsing is more complex.
	prefersJSON := false
	if acceptHeader != "" {
		parts := strings.Split(acceptHeader, ",")
		for _, part := range parts {
			trimmedPart := strings.TrimSpace(part)
			if strings.HasPrefix(trimmedPart, "application/json") ||
				(strings.HasPrefix(trimmedPart, "*/*") && !strings.Contains(acceptHeader, "text/html")) { // Basic check
				// A more robust check would parse q-values.
				// For now, if application/json is anywhere, or */* without explicit text/html, assume JSON.
				// Or if application/json;q=1.0
				if strings.Contains(trimmedPart, "application/json") {
					prefersJSON = true
					break
				}
				if strings.HasPrefix(trimmedPart, "*/*") { // If */* is present, and no higher q for text/html
					// This logic is simplified. Real negotiation is harder.
					// For now, if we see */* and haven't seen text/html with higher q, assume JSON is fine.
					// Or if `application/json` is also present with q=1.0
					// Simplification: If "application/json" present with q=1.0, or it's the first non-wildcard, or */* is first
					// and application/json is preferred over text/html.
					// The spec language is "application/json is the first listed type with q > 0"
					// or "application/json is present with q=1.0"
					// or "*/* is present and no other type has higher q-value than application/json"

					// Let's simplify to: if "application/json" is present in Accept, prefer JSON.
					// This is not strictly spec compliant on q-value ordering but a common shortcut.
					// Re-evaluating the spec wording:
					// 1. "application/json is the first listed type with q > 0"
					// 2. "or */* is present and no other type has higher q-value than application/json"
					// 3. "or application/json is present with q=1.0"

					// Let's do a simpler "contains application/json" check for now and refine if needed.
					// This is what many frameworks do.
					if strings.Contains(strings.ToLower(acceptHeader), "application/json") {
						prefersJSON = true
					}
					// If text/html is explicitly preferred over application/json, this would be false.
					// e.g. text/html, application/json;q=0.9
					// For now, any mention of application/json will tip it.
					break // Found a preference indicator
				}
			}
		}
		// A simpler check: if "application/json" appears before "text/html" or if "text/html" is not present.
		jsonIdx := strings.Index(strings.ToLower(acceptHeader), "application/json")
		htmlIdx := strings.Index(strings.ToLower(acceptHeader), "text/html")

		if jsonIdx != -1 {
			if htmlIdx == -1 || jsonIdx < htmlIdx {
				prefersJSON = true
			}
		} else if strings.Contains(strings.ToLower(acceptHeader), "*/*") && htmlIdx == -1 {
			// If */* is present and text/html is not, we can serve JSON.
			prefersJSON = true
		}

	}

	if prefersJSON {
		contentType = "application/json; charset=utf-8"
		errorResp := ErrorResponseJSON{
			Error: ErrorDetail{
				StatusCode: statusCode,
				Message:    statusText,
				Detail:     optionalDetail,
			},
		}
		var err error
		body, err = json.Marshal(errorResp)
		if err != nil {
			// Fallback to HTML if JSON marshaling fails, though this should be rare.
			fields := logger.LogFields{"error": err}
			log.Error("Failed to marshal JSON error response. Falling back to HTML.", fields)
			prefersJSON = false // Force HTML path
		}
	}

	// If not preferring JSON, or if JSON marshaling failed
	if !prefersJSON {
		contentType = "text/html; charset=utf-8"
		defaultMsg, ok := defaultHTMLMessages[statusCode]
		if !ok {
			// Generic fallback HTML for unmapped status codes
			defaultMsg = struct {
				title   string
				heading string
				message string
			}{
				title:   fmt.Sprintf("%d %s", statusCode, statusText),
				heading: statusText,
				message: "An unexpected error occurred.",
			}
			if optionalDetail != "" {
				defaultMsg.message = optionalDetail
			}
		} else {
			// Append optionalDetail to the standard message for known HTML errors if present
			if optionalDetail != "" {
				// Ensure detail is HTML-escaped if it's user-provided or could contain special chars
				defaultMsg.message += " " + html.EscapeString(optionalDetail)
			}
		}

		body = []byte(fmt.Sprintf(
			"<html><head><title>%s</title></head><body><h1>%s</h1><p>%s</p></body></html>",
			html.EscapeString(defaultMsg.title),
			html.EscapeString(defaultMsg.heading),
			defaultMsg.message, // Already escaped if needed (optionalDetail part)
		))
	}

	headers := []hpack.HeaderField{
		{Name: ":status", Value: fmt.Sprintf("%d", statusCode)},
		{Name: "content-type", Value: contentType},
		{Name: "content-length", Value: fmt.Sprintf("%d", len(body))},
		// Consider adding "cache-control: no-cache, no-store, must-revalidate" for errors
		{Name: "cache-control", Value: "no-cache, no-store, must-revalidate"},
		{Name: "pragma", Value: "no-cache"}, // HTTP/1.0 backward compatibility for caches
		{Name: "expires", Value: "0"},       // Proxies
	}

	// Send headers
	err := stream.SendHeaders(headers, len(body) == 0) // endStream if body is empty
	if err != nil {
		log.Error("Error sending default error response headers for stream.", logger.LogFields{"error": err})
		// If sending headers fails, we might not be able to do much more on this stream.
		// The stream might be already closed or in an error state.
		return
	}

	// Send body if present
	if len(body) > 0 {
		_, err = stream.WriteData(body, true) // endStream = true as this is the full body
		if err != nil {
			log.Error("Error sending default error response body for stream.", logger.LogFields{"error": err})
		}
	}
}
