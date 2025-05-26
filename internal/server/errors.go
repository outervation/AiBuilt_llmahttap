package server

import (
	"encoding/json"
	"example.com/llmahttap/v2/internal/http2"
	"fmt"
	"html"
	"mime"
	"net/http"
	"strconv"
	"strings"

	"example.com/llmahttap/v2/internal/logger"
	"golang.org/x/net/http2/hpack"
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
		return false
	}

	type offer struct {
		mediaType  string
		q          float64
		isJson     bool
		isHtml     bool
		isAppStar  bool
		isStarStar bool
	}

	rawOffers := strings.Split(acceptHeaderValue, ",")
	parsedOffers := make([]offer, 0, len(rawOffers))

	for _, partStr := range rawOffers {
		trimmedPart := strings.TrimSpace(partStr)
		if trimmedPart == "" {
			continue
		}
		mediaType, params, err := mime.ParseMediaType(trimmedPart)
		if err != nil {
			continue
		}
		qVal := 1.0
		if qStr, ok := params["q"]; ok {
			qVal = parseQValue(qStr)
		}
		if qVal == 0.0 {
			continue
		}
		lowerMediaType := strings.ToLower(mediaType)
		parsedOffers = append(parsedOffers, offer{
			mediaType:  lowerMediaType,
			q:          qVal,
			isJson:     lowerMediaType == "application/json",
			isHtml:     lowerMediaType == "text/html",
			isAppStar:  lowerMediaType == "application/*",
			isStarStar: lowerMediaType == "*/*",
		})
	}

	if len(parsedOffers) == 0 {
		return false
	}

	ruleA_satisfied := false
	if len(parsedOffers) > 0 {
		firstOffer := parsedOffers[0]
		if (firstOffer.isJson || firstOffer.isAppStar) && firstOffer.q > 0 {
			ruleA_satisfied = true
		}
	}
	if ruleA_satisfied {
		return true
	}

	ruleC_satisfied := false
	for _, o := range parsedOffers {
		if o.isJson && o.q == 1.0 {
			ruleC_satisfied = true
			break
		}
	}
	if ruleC_satisfied {
		return true
	}

	ruleB_satisfied := false
	starStarPresent := false
	highestQExplicitAppJson := 0.0
	highestQAppStar := 0.0
	highestQStarStar := 0.0

	for _, o := range parsedOffers {
		if o.isStarStar {
			starStarPresent = true
			if o.q > highestQStarStar {
				highestQStarStar = o.q
			}
		}
		if o.isJson {
			if o.q > highestQExplicitAppJson {
				highestQExplicitAppJson = o.q
			}
		}
		if o.isAppStar {
			if o.q > highestQAppStar {
				highestQAppStar = o.q
			}
		}
	}

	if starStarPresent {
		effectiveQJsonForRuleB := 0.0
		if highestQExplicitAppJson > effectiveQJsonForRuleB {
			effectiveQJsonForRuleB = highestQExplicitAppJson
		}
		if highestQAppStar > effectiveQJsonForRuleB {
			effectiveQJsonForRuleB = highestQAppStar
		}
		if highestQStarStar > effectiveQJsonForRuleB {
			effectiveQJsonForRuleB = highestQStarStar
		}

		allOtherSpecificTypesNotHigher := true
		for _, o := range parsedOffers {
			if !o.isJson && !o.isAppStar && !o.isStarStar {
				if o.q > effectiveQJsonForRuleB {
					allOtherSpecificTypesNotHigher = false
					break
				}
			}
		}
		if allOtherSpecificTypesNotHigher {
			ruleB_satisfied = true
		}
	}

	if ruleB_satisfied {
		return true
	}

	return false
}

// WriteErrorResponse generates and sends a default HTTP error response on the given stream.
func WriteErrorResponse(stream ErrorResponseWriterStream, statusCode int, requestHeaders []http2.HeaderField, detailMessage string, log *logger.Logger) error {
	if log != nil { // Defend against nil logger, though it should be guaranteed
		log.Debug("WriteErrorResponse: ENTERED", logger.LogFields{
			"status_code": statusCode,
			"detail":      detailMessage,
			"stream_id":   StreamID(stream),
		})
	}
	statusText := http.StatusText(statusCode)
	if statusText == "" {
		statusText = "Error" // Default for unknown codes
	}

	acceptHeaderValue := ""
	for _, hf := range requestHeaders {
		// Ensure correct case-insensitive comparison for header names
		if strings.ToLower(hf.Name) == "accept" {
			acceptHeaderValue = hf.Value
			break
		}
	}

	var body []byte
	var contentType string
	jsonMarshalFailed := false

	shouldSendJSON := PrefersJSON(acceptHeaderValue)

	if shouldSendJSON {
		contentType = "application/json; charset=utf-8"
		errorResp := ErrorResponseJSON{
			Error: ErrorDetail{
				StatusCode: statusCode,
				Message:    statusText,
				Detail:     detailMessage,
			},
		}
		var marshalErr error
		body, marshalErr = jsonMarshalFunc(errorResp) // Use the swappable marshal func
		if marshalErr != nil {
			if log != nil {
				log.Error("Failed to marshal JSON error response, falling back to HTML.", logger.LogFields{"error": marshalErr, "statusCode": statusCode})
			}
			jsonMarshalFailed = true // Mark as failed to force HTML fallback
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
			baseMessage = "The server encountered an error processing your request." // Generic message for unknown codes
		}

		htmlSafeMessageBody := baseMessage
		if detailMessage != "" {
			escapedDetail := html.EscapeString(detailMessage)
			// For unknown codes, the detailMessage might be the only specific info.
			// For known codes, append it if present.
			if !isKnownCode {
				htmlSafeMessageBody = escapedDetail // Use detail as main message if code is unknown
			} else {
				htmlSafeMessageBody = baseMessage + " " + escapedDetail
			}
		}
		// Ensure GenerateHTMLResponseBodyForTest is used correctly; it's a helper for tests
		// but can be used here if its output is suitable for production default error pages.
		// Spec examples for 404/500:
		// <html><head><title>404 Not Found</title></head><body><h1>Not Found</h1><p>The requested resource was not found on this server.</p></body></html>
		// Let's use a similar structure, calling the test helper for consistency.
		body = GenerateHTMLResponseBodyForTest(finalTitle, finalHeading, htmlSafeMessageBody)
	}

	responseHPACKHeaders := []hpack.HeaderField{
		{Name: ":status", Value: strconv.Itoa(statusCode)},
		{Name: "content-type", Value: contentType},
		{Name: "content-length", Value: strconv.Itoa(len(body))},
		// Per spec for error responses, caching should usually be prevented.
		{Name: "cache-control", Value: "no-cache, no-store, must-revalidate"},
		{Name: "pragma", Value: "no-cache"}, // HTTP/1.0 backward compatibility for Cache-Control
		{Name: "expires", Value: "0"},       // Proxies
	}

	http2ResponseHeaders := hpackHeadersToHttp2Headers(responseHPACKHeaders)
	err := stream.SendHeaders(http2ResponseHeaders, len(body) == 0)
	if err != nil {
		if log != nil {
			log.Error("Failed to send error response headers.", logger.LogFields{"error": err, "streamID": StreamID(stream), "statusCode": statusCode})
		}
		return fmt.Errorf("failed to send error response headers (status %d) for stream %v: %w", statusCode, StreamID(stream), err)
	}

	if len(body) > 0 {
		_, err = stream.WriteData(body, true)
		if err != nil {
			if log != nil {
				log.Error("Failed to send error response body.", logger.LogFields{"error": err, "streamID": StreamID(stream), "statusCode": statusCode})
			}
			return fmt.Errorf("failed to send error response body (status %d) for stream %v: %w", statusCode, StreamID(stream), err)
		}
	}
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
