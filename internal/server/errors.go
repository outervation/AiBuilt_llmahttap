package server

import (
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"example.com/llmahttap/v2/internal/http2" // For HeaderField type in function signatures
	"example.com/llmahttap/v2/internal/logger"
	"golang.org/x/net/http2/hpack" // For actual hpack.HeaderField used in hpackHeadersToHttp2Headers
)

// jsonMarshalFunc allows swapping out json.Marshal for testing.
var jsonMarshalFunc = json.Marshal

// ErrorDetail represents the inner structure of a JSON error response.
type ErrorDetail struct {
	StatusCode int    `json:"status_code"`
	Message    string `json:"message"`
	Detail     string `json:"detail,omitempty"`
}

// ErrorResponseJSON represents the full JSON error response body.
type ErrorResponseJSON struct {
	Error ErrorDetail `json:"error"`
}

// defaultHTMLMessages maps HTTP status codes to their default HTML messages.
var defaultHTMLMessages = map[int]struct {
	Title   string
	Heading string
	Message string
}{
	http.StatusNotFound: {
		Title:   "404 Not Found",
		Heading: "Not Found",
		Message: "The requested resource was not found on this server.",
	},
	http.StatusInternalServerError: {
		Title:   "500 Internal Server Error",
		Heading: "Internal Server Error",
		Message: "The server encountered an internal error and was unable to complete your request.",
	},
	http.StatusForbidden: {
		Title:   "403 Forbidden",
		Heading: "Forbidden",
		Message: "You do not have permission to access this resource.",
	},
	http.StatusMethodNotAllowed: {
		Title:   "405 Method Not Allowed",
		Heading: "Method Not Allowed",
		Message: "The method specified in the Request-Line is not allowed for the resource identified by the Request-URI.",
	},
	http.StatusBadRequest: {
		Title:   "400 Bad Request",
		Heading: "Bad Request",
		Message: "The server cannot or will not process the request due to an apparent client error.",
	},
}

// hpackHeadersToHttp2Headers converts []hpack.HeaderField to []http2.HeaderField.
func hpackHeadersToHttp2Headers(hpackHeaders []hpack.HeaderField) []http2.HeaderField {
	if hpackHeaders == nil {
		return nil
	}
	http2Headers := make([]http2.HeaderField, len(hpackHeaders))
	for i, hf := range hpackHeaders {
		http2Headers[i] = http2.HeaderField{Name: hf.Name, Value: hf.Value}
	}
	return http2Headers
}

// anOffer is a helper struct to store parsed media type offers from an Accept header.
type anOffer struct {
	mediaType string
	q         float64
}

// parseQValue parses a q-value string from an Accept header.
func parseQValue(qStr string) float64 {
	q, err := strconv.ParseFloat(qStr, 64)
	if err != nil {
		return 0.0
	}
	if q < 0.0 {
		return 0.0
	}
	if q > 1.0 {
		return 1.0
	}
	return q
}

// PrefersJSON checks if the client prefers application/json based on the Accept header.
func PrefersJSON(acceptHeaderValue string) bool {
	if acceptHeaderValue == "" {
		return false // Default to HTML
	}

	// Offers: store media types and their q-values
	type offer struct {
		mediaType string
		q         float64
		specific  bool // true if not wildcard type (e.g., application/json vs application/* or */*)
		order     int  // original order in header
	}
	var offers []offer

	rawParts := strings.Split(acceptHeaderValue, ",")
	for i, partStr := range rawParts {
		partStr = strings.TrimSpace(partStr)
		mediaType := partStr
		qValue := 1.0 // Default q-value is 1.0

		if idx := strings.Index(partStr, ";"); idx != -1 {
			mediaType = strings.TrimSpace(partStr[:idx])
			// Look for q-value parameter
			// A full parser would handle all parameters, but we only care about 'q'.
			// Example: "text/html; q=0.8" or "text/html;level=1;q=0.8"
			paramsStr := strings.TrimSpace(partStr[idx+1:])
			paramParts := strings.Split(paramsStr, ";")
			for _, param := range paramParts {
				param = strings.TrimSpace(param)
				if strings.HasPrefix(param, "q=") {
					qStr := param[2:]
					if q, err := strconv.ParseFloat(qStr, 64); err == nil {
						if q >= 0 && q <= 1 { // q must be between 0 and 1
							qValue = q
						} else {
							qValue = 0 // Invalid q is treated as 0 (effectively rejected unless it's the only option)
						}
					} else {
						qValue = 0 // Parse error for q is also treated as 0
					}
					break // Found q, no need to check other params for this media type part
				}
			}
		}

		// Per RFC 7231 Section 5.3.2: a sender that generates a media type with a qvalue of 0 MUST NOT generate that media type.
		// A recipient that receives a media type with a qvalue of 0 MUST ignore that media type.
		if qValue > 0 {
			offers = append(offers, offer{
				mediaType: strings.ToLower(mediaType), // Media types are case-insensitive
				q:         qValue,
				specific:  !strings.HasSuffix(mediaType, "/*") && mediaType != "*/*",
				order:     i,
			})
		}
	}

	if len(offers) == 0 { // All types had q=0 or header was malformed leading to no valid offers
		return false // Default to HTML
	}

	// Sort offers:
	// 1. Higher q-value first.
	// 2. More specific (application/json > application/* > */*).
	//    Specificity: concrete > partial wildcard > full wildcard.
	// 3. Original order in header (lower index first).
	sort.Slice(offers, func(i, j int) bool {
		if offers[i].q != offers[j].q {
			return offers[i].q > offers[j].q // Higher q-value first
		}
		// Specificity: A specific type (specific:true) is preferred over a wildcard type (specific:false).
		// application/json vs application/* : json is more specific
		// application/* vs */* : application/* is more specific
		if offers[i].specific != offers[j].specific {
			return offers[i].specific // true (specific) comes before false (wildcard)
		}
		// If q-values and specificity are equal, prefer the one that appeared earlier in the header.
		return offers[i].order < offers[j].order
	})

	// After sorting, the first element in `offers` is the most preferred type.
	// We return true if this most preferred type is "application/json".
	// This directly addresses the spec: "If the Accept header indicates a preference for application/json..."
	// It correctly handles "Accept: */*" (results in HTML because */* is not application/json)
	// and "Accept: application/json, */*" (results in JSON because application/json is more specific and thus preferred).
	// It also covers "application/json is the first listed type with q > 0" and "q=1.0" by virtue of the sorting.
	// The "no other type has higher q-value than application/json" for */* is effectively handled because
	// if */* were to result in JSON, application/json would have to be the effective best match anyway.
	return offers[0].mediaType == "application/json"
}

// WriteErrorResponse generates and sends a default HTTP error response on the given stream.

func WriteErrorResponse(stream ErrorResponseWriterStream, statusCode int, requestHeaders []http2.HeaderField, detailMessage string, log *logger.Logger) error {
	if log == nil {
		log = logger.NewDiscardLogger() // Prevent nil panics if logger not provided
	}
	log.Debug("WriteErrorResponse: ENTERED", logger.LogFields{"stream_id": StreamID(stream), "status_code": statusCode, "detail": detailMessage})

	acceptHeader := ""
	for _, h := range requestHeaders {
		// HTTP/2 header names are conventionally lowercase.
		// The hpack decoder ensures this.
		if h.Name == "accept" {
			acceptHeader = h.Value
			break
		}
	}

	var body []byte
	var contentType string
	responseHPACKHeaders := []hpack.HeaderField{ // Using hpack.HeaderField as this is what SendHeaders usually expects or converts from
		{Name: ":status", Value: strconv.Itoa(statusCode)},
		// Date header is usually added by HTTP libraries or connection manager automatically
		// Cache-Control headers for errors:
		{Name: "cache-control", Value: "no-cache, no-store, must-revalidate"},
		{Name: "pragma", Value: "no-cache"}, // HTTP/1.0 backward compatibility
		{Name: "expires", Value: "0"},       // Proxies
	}

	statusText := http.StatusText(statusCode)
	if statusText == "" {
		statusText = "Unknown Status" // Default for unknown codes
	}

	if statusCode == http.StatusMethodNotAllowed {
		// Spec 2.3.2: For StaticFileServer, 405 must include Allow: GET, HEAD, OPTIONS
		// This function is generic, but the primary user (StaticFileServer) requires this.
		// A more advanced system might pass allowedMethods through context or specific error types.
		// For now, we hardcode the StaticFileServer's requirement as it's a common case.
		responseHPACKHeaders = append(responseHPACKHeaders, hpack.HeaderField{Name: "allow", Value: "GET, HEAD, OPTIONS"})
		log.Debug("WriteErrorResponse: Added Allow header for 405", logger.LogFields{"stream_id": StreamID(stream)})
	}

	jsonMarshalFailed := false
	shouldSendJSON := PrefersJSON(acceptHeader)

	if shouldSendJSON {
		contentType = "application/json; charset=utf-8"
		errorResp := ErrorResponseJSON{
			Error: ErrorDetail{
				StatusCode: statusCode,
				Message:    statusText,
				Detail:     detailMessage,
			},
		}
		jsonBody, err := jsonMarshalFunc(errorResp) // Use the swappable marshal func
		if err != nil {
			log.Error("WriteErrorResponse: Failed to marshal JSON error response, falling back to HTML.", logger.LogFields{"stream_id": StreamID(stream), "error": err.Error(), "statusCode": statusCode})
			jsonMarshalFailed = true // Mark as failed to force HTML fallback
		} else {
			body = jsonBody
		}
	}

	// Fallback to HTML if JSON was not preferred, or if JSON marshalling failed
	if !shouldSendJSON || jsonMarshalFailed {
		contentType = "text/html; charset=utf-8"
		var finalTitle, finalHeading, baseMessage string
		defaultMsgData, isKnownCode := defaultHTMLMessages[statusCode]

		if isKnownCode {
			finalTitle = defaultMsgData.Title
			finalHeading = defaultMsgData.Heading
			baseMessage = defaultMsgData.Message
		} else {
			finalTitle = fmt.Sprintf("%d %s", statusCode, statusText)
			finalHeading = statusText
			baseMessage = "The server encountered an error processing your request."
		}

		htmlSafeMessageBody := baseMessage
		if detailMessage != "" {
			escapedDetail := html.EscapeString(detailMessage)
			if !isKnownCode { // For unknown codes, detail might be the only specific info
				htmlSafeMessageBody = escapedDetail
			} else {
				htmlSafeMessageBody = baseMessage + " " + escapedDetail
			}
		}
		// Using GenerateHTMLResponseBodyForTest to maintain consistency with how testable bodies are made.
		body = GenerateHTMLResponseBodyForTest(finalTitle, finalHeading, htmlSafeMessageBody)

		if jsonMarshalFailed { // If we are here due to json marshal failure, log it as a warning for the fallback.
			log.Warn("WriteErrorResponse: Fell back to HTML response due to JSON marshal error.", logger.LogFields{"stream_id": StreamID(stream), "statusCode": statusCode})
		}
	}

	responseHPACKHeaders = append(responseHPACKHeaders, hpack.HeaderField{Name: "content-type", Value: contentType})
	responseHPACKHeaders = append(responseHPACKHeaders, hpack.HeaderField{Name: "content-length", Value: strconv.Itoa(len(body))})

	http2ResponseHeaders := hpackHeadersToHttp2Headers(responseHPACKHeaders)

	log.Debug("WriteErrorResponse: Sending headers", logger.LogFields{"stream_id": StreamID(stream), "num_headers": len(http2ResponseHeaders), "headers": http2ResponseHeaders})
	err := stream.SendHeaders(http2ResponseHeaders, len(body) == 0)
	if err != nil {
		log.Error("WriteErrorResponse: Failed to send error headers", logger.LogFields{"stream_id": StreamID(stream), "error": err.Error(), "statusCode": statusCode})
		return fmt.Errorf("failed to send error response headers (status %d) for stream %v: %w", statusCode, StreamID(stream), err)
	}

	if len(body) > 0 {
		log.Debug("WriteErrorResponse: Writing body", logger.LogFields{"stream_id": StreamID(stream), "body_len": len(body)})
		_, err = stream.WriteData(body, true)
		if err != nil {
			log.Error("WriteErrorResponse: Failed to write error body", logger.LogFields{"stream_id": StreamID(stream), "error": err.Error(), "statusCode": statusCode})
			return fmt.Errorf("failed to send error response body (status %d) for stream %v: %w", statusCode, StreamID(stream), err)
		}
	}
	log.Debug("WriteErrorResponse: Successfully sent error response", logger.LogFields{"stream_id": StreamID(stream), "statusCode": statusCode})
	return nil
}

// writeMinimalHTMLFallback is used if JSON marshaling fails during error response generation.
// THIS FUNCTION IS NO LONGER DIRECTLY CALLED as the main function now handles fallback logic internally.
// It's kept here for reference or if a very minimal path is needed, but current logic in WriteErrorResponse makes it redundant.
// For production code, if unused, this could be removed. For now, it's harmless.
// The original `RAWTEXT` included this, so I've kept its structure but noted its current unused status
// in the context of the modified `WriteErrorResponse` above.
// If `WriteErrorResponse` were to simplify and delegate fallbacks, this could be revived.
// Based on the new `WriteErrorResponse`, this function is effectively dead code.
// However, the prompt asks to fix syntax, not refactor the LLM's provided code logic beyond making it work.
// The LLM's provided `WriteErrorResponse` in the RAWTEXT *does not* call this function.
// The logic for fallback is *inside* `WriteErrorResponse`.
// So this function, as provided in the original `RAWTEXT`, is indeed not called by the accompanying `WriteErrorResponse`.
func writeMinimalHTMLFallback(stream ErrorResponseWriterStream, statusCode int, fallbackMessage string, statusText string, log *logger.Logger) error {
	log.Warn("writeMinimalHTMLFallback: Invoked. This indicates a direct call, bypassing main error path or for specific minimal cases.",
		logger.LogFields{"stream_id": StreamID(stream), "statusCode": statusCode, "fallbackMessage": fallbackMessage})

	contentType := "text/html; charset=utf-8" // Corrected charset from original rawtext which had utf-utf-8
	title := fmt.Sprintf("%d %s", statusCode, statusText)
	bodyHTML := fmt.Sprintf(
		"<html><head><title>%s</title></head><body><h1>%s</h1><p>%s</p></body></html>",
		html.EscapeString(title), html.EscapeString(statusText), html.EscapeString(fallbackMessage),
	)
	body := []byte(bodyHTML)

	hpackHeaders := []hpack.HeaderField{
		{Name: ":status", Value: strconv.Itoa(statusCode)},
		{Name: "content-type", Value: contentType},
		{Name: "content-length", Value: strconv.Itoa(len(body))},
		{Name: "cache-control", Value: "no-cache, no-store, must-revalidate"},
		{Name: "pragma", Value: "no-cache"},
		{Name: "expires", Value: "0"},
	}
	if statusCode == http.StatusMethodNotAllowed {
		hpackHeaders = append(hpackHeaders, hpack.HeaderField{Name: "allow", Value: "GET, HEAD, OPTIONS"})
	}

	http2Headers := hpackHeadersToHttp2Headers(hpackHeaders)

	err := stream.SendHeaders(http2Headers, len(body) == 0)
	if err != nil {
		log.Error("writeMinimalHTMLFallback: Failed to send HTML fallback error headers", logger.LogFields{"stream_id": StreamID(stream), "error": err.Error()})
		return err
	}
	if len(body) > 0 {
		_, err = stream.WriteData(body, true)
		if err != nil {
			log.Error("writeMinimalHTMLFallback: Failed to write HTML fallback error body", logger.LogFields{"stream_id": StreamID(stream), "error": err.Error()})
			return err
		}
	}
	log.Debug("writeMinimalHTMLFallback: Successfully sent minimal HTML fallback response.", logger.LogFields{"stream_id": StreamID(stream)})
	return nil
}

// SendDefaultErrorResponse generates and sends a default HTTP error response using WriteErrorResponse.
// req can be nil if the error is not request-bound or if request details are unavailable.
func SendDefaultErrorResponse(stream ErrorResponseWriterStream, statusCode int, req *http.Request, optionalDetail string, log *logger.Logger) {
	var reqHeaders []http2.HeaderField

	if req != nil {
		// Convert http.Request.Header (map[string][]string) to []http2.HeaderField
		// This is a simplified conversion; real http.Header can have multiple values for one key.
		// For Accept, typically only the first value is most significant for simple parsing.
		// We only need the "accept" header.
		if acceptVal := req.Header.Get("Accept"); acceptVal != "" {
			reqHeaders = append(reqHeaders, http2.HeaderField{Name: "accept", Value: acceptVal})
		}
		// Other headers from req are not directly needed by WriteErrorResponse's current logic,
		// but passing them all might be more robust if WriteErrorResponse evolves.
		// For now, just "accept".
	} else {
		// If req is nil, we cannot determine the Accept header.
		// WriteErrorResponse will default to HTML in this case.
		if log != nil && statusCode != http.StatusNotFound { // 404s are common and might not have full req context early
			log.Debug("SendDefaultErrorResponse called with nil http.Request, Accept header unknown, will default to HTML error response.",
				logger.LogFields{"streamID": StreamID(stream), "statusCode": statusCode})
		}
	}

	// Call the main WriteErrorResponse function.
	// It handles content negotiation based on the "accept" header in reqHeaders.
	err := WriteErrorResponse(stream, statusCode, reqHeaders, optionalDetail, log)
	if err != nil {
		// WriteErrorResponse already logs its internal errors.
		// This log is for the fact that SendDefaultErrorResponse encountered an issue via WriteErrorResponse.
		if log != nil {
			log.Error("Error occurred within WriteErrorResponse called by SendDefaultErrorResponse.",
				logger.LogFields{"error": err, "streamID": StreamID(stream), "statusCode": statusCode})
		}
		// If sending the error response itself fails, there's not much more to do on this stream.
		// The connection might be compromised. The caller of SendDefaultErrorResponse might
		// need to initiate connection closure if this error is severe (e.g. network error).
	}
}

// GenerateHTMLResponseBodyForTest creates a simple HTML error page.
func GenerateHTMLResponseBodyForTest(title, heading, message string) []byte {
	titleEsc := html.EscapeString(title)
	headingEsc := html.EscapeString(heading)
	body := fmt.Sprintf(`<html><head><title>%s</title></head><body><h1>%s</h1><p>%s</p></body></html>`, titleEsc, headingEsc, message)
	return []byte(body)
}

// StreamID tries to extract stream ID from ResponseWriter if it's a Stream.
func StreamID(s ErrorResponseWriterStream) interface{} {
	if st, ok := s.(interface{ ID() uint32 }); ok {
		return st.ID()
	}
	return "unknown"
}

// TestingOnlySetJSONMarshal is used by tests to mock json.Marshal behavior.
func TestingOnlySetJSONMarshal(fn func(v interface{}) ([]byte, error)) func(v interface{}) ([]byte, error) {
	original := jsonMarshalFunc
	jsonMarshalFunc = fn
	return original
}

// GetDefaultHTMLMessageInfo is used by tests to access default HTML message components.
func GetDefaultHTMLMessageInfo(statusCode int) (info struct {
	Title   string
	Heading string
	Message string
}, found bool) {
	info, found = defaultHTMLMessages[statusCode]
	return
}
