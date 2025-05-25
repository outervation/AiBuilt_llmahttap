package server

import (
	"encoding/json"
	"fmt"
	"html"
	"mime"
	"net/http"
	"strconv"
	"strings"

	"example.com/llmahttap/v2/internal/http2" // This is used by SendDefaultErrorResponse and WriteErrorResponse type signature
	"example.com/llmahttap/v2/internal/logger"

	"golang.org/x/net/http2/hpack"
)

// jsonMarshalFunc allows swapping out json.Marshal for testing.
var jsonMarshalFunc = json.Marshal

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
	// Add more as needed
}

// anOffer is a helper struct to store parsed media type offers from an Accept header.
type anOffer struct {
	mediaType string
	q         float64
}

// parseQValue parses a q-value string from an Accept header.
// It returns a float64 between 0.0 and 1.0.
// Invalid q-values (non-numeric, <0) are treated as 0.0.
// Q-values >1.0 are treated as 1.0.
// This aligns with RFC 9110, Section 12.5.1.
func parseQValue(qStr string) float64 {
	q, err := strconv.ParseFloat(qStr, 64)
	if err != nil {
		return 0.0 // Not a valid number
	}
	if q < 0.0 {
		return 0.0
	}
	if q > 1.0 {
		return 1.0
	}
	return q
}

// prefersJSON checks if the client prefers application/json based on the Accept header.
// Implements logic from Feature Spec 5.2.1:
// - "application/json is the first listed type with q > 0"
// - "or */* is present and no other type has higher q-value than application/json"
// - "or application/json is present with q=1.0"
func PrefersJSON(acceptHeaderValue string) bool {
	if acceptHeaderValue == "" {
		return false // No Accept header, default to HTML
	}

	type offer struct {
		mediaType string
		q         float64
		// index      int // Original index for tie-breaking - not strictly needed for this rule evaluation
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
			continue // Skip malformed
		}
		qVal := 1.0
		if qStr, ok := params["q"]; ok {
			qVal = parseQValue(qStr)
		}
		if qVal == 0.0 { // q=0 means "not acceptable"
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
		return false // All malformed or q=0
	}

	// Rule A: "application/json is the first listed type with q > 0" (or application/*)
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

	// Rule C: "application/json is present with q=1.0"
	ruleC_satisfied := false
	for _, o := range parsedOffers {
		if o.isJson && o.q == 1.0 { // Strictly application/json for Rule C
			ruleC_satisfied = true
			break
		}
	}
	if ruleC_satisfied {
		return true
	}

	// Rule B: "or */* is present and no other type has higher q-value than application/json"
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
		// Determine the effective q-value for "application/json" interest in Rule B.
		// This includes q-values from application/json, application/*, and */*.
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
			if !o.isJson && !o.isAppStar && !o.isStarStar { // It's a specific type other than JSON-likes and wildcard
				if o.q > effectiveQJsonForRuleB { // Compare against the new effective Q
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

	return false // None of A, B, or C were met
}

// WriteErrorResponse generates and sends a default HTTP error response on the given stream.
// It performs content negotiation based on the Accept header found in requestHeaders.
//
// stream: The HTTP/2 stream to send the response on.
// statusCode: The HTTP status code for the error.
// requestHeaders: The received request headers, used to extract the Accept header.
// detailMessage: An optional, more specific message to include in the error response.
//
//	For JSON, this populates the "detail" field.
//	For HTML, this is HTML-escaped and used to provide more specific information
//	either as the main message (for unknown status codes) or appended to the
//	standard message (for known status codes).
//
// Returns an error if sending the response fails. If JSON marshalling fails,
// it gracefully falls back to sending an HTML response.
func WriteErrorResponse(stream http2.ResponseWriter, statusCode int, requestHeaders []hpack.HeaderField, detailMessage string) error {
	statusText := http.StatusText(statusCode)
	if statusText == "" {
		statusText = "Error" // Fallback for unknown status codes
	}

	acceptHeaderValue := ""
	for _, hf := range requestHeaders {
		// HTTP/2 header names should be lowercase.
		if hf.Name == "accept" { // More direct check as HPACK decodes to lowercase.
			acceptHeaderValue = hf.Value
			break
		} else if strings.ToLower(hf.Name) == "accept" { // Fallback for robustness
			acceptHeaderValue = hf.Value
			break
		}
	}

	var body []byte
	var contentType string
	jsonMarshalFailed := false

	// Determine if JSON response is preferred
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
		body, marshalErr = jsonMarshalFunc(errorResp)
		if marshalErr != nil {
			// JSON marshaling failed. Per spec, if "SHOULD" send JSON cannot be met,
			// then "MUST" send HTML (the "Otherwise" condition).
			jsonMarshalFailed = true
			// Note: The marshalErr itself is not returned here, as the function's goal
			// is to send *an* error response. The caller can log if this function returns an error
			// during the actual send operation.
		}
	}

	// Fallback to HTML if JSON was not preferred, or if JSON marshaling failed
	if !shouldSendJSON || jsonMarshalFailed {
		contentType = "text/html; charset=utf-8"

		var finalTitle, finalHeading, baseMessage string
		defaultMsgData, isKnownCode := defaultHTMLMessages[statusCode]

		if isKnownCode {
			finalTitle = defaultMsgData.Title
			finalHeading = defaultMsgData.Heading
			baseMessage = defaultMsgData.Message
		} else { // Generic error for unknown status codes
			finalTitle = fmt.Sprintf("%d %s", statusCode, statusText)
			finalHeading = statusText
			baseMessage = "The server encountered an error." // Default simple message for unknown codes
		}

		// Construct the final HTML-safe message body string
		htmlSafeMessageBody := baseMessage
		if detailMessage != "" {
			escapedDetail := html.EscapeString(detailMessage)
			if !isKnownCode {
				// For generic/unknown errors, if detail is present, it becomes the primary message.
				htmlSafeMessageBody = escapedDetail
			} else {
				// For known errors, if detail is present, it's appended to the base message.
				htmlSafeMessageBody = baseMessage + " " + escapedDetail
			}
		}

		body = GenerateHTMLResponseBodyForTest(finalTitle, finalHeading, htmlSafeMessageBody)
	}

	responseHPACKHeaders := []hpack.HeaderField{
		{Name: ":status", Value: strconv.Itoa(statusCode)},
		{Name: "content-type", Value: contentType},
		{Name: "content-length", Value: strconv.Itoa(len(body))},
		{Name: "cache-control", Value: "no-cache, no-store, must-revalidate"},
		{Name: "pragma", Value: "no-cache"}, // For HTTP/1.0 proxies/clients
		{Name: "expires", Value: "0"},       // For proxies
	}

	err := stream.SendHeaders(responseHPACKHeaders, len(body) == 0)
	if err != nil {
		return fmt.Errorf("failed to send error response headers (status %d): %w", statusCode, err)
	}

	if len(body) > 0 {
		_, err = stream.WriteData(body, true)
		if err != nil {
			return fmt.Errorf("failed to send error response body (status %d): %w", statusCode, err)
		}
	}
	return nil
}

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
		acceptHeader = req.Header.Get("Accept")
	}

	var body []byte
	var contentType string

	// TODO: This simplified JSON preference check should be replaced by: if prefersJSON(acceptHeader)
	// For now, keeping the old logic as per "Do NOT make other changes yet" for SendDefaultErrorResponse.
	prefersJSONCurrentLogic := false
	if acceptHeader != "" {
		jsonIdx := strings.Index(strings.ToLower(acceptHeader), "application/json")
		htmlIdx := strings.Index(strings.ToLower(acceptHeader), "text/html")

		if jsonIdx != -1 {
			if htmlIdx == -1 || jsonIdx < htmlIdx {
				prefersJSONCurrentLogic = true
			}
		} else if strings.Contains(strings.ToLower(acceptHeader), "*/*") && htmlIdx == -1 {
			prefersJSONCurrentLogic = true
		}
	}

	if prefersJSONCurrentLogic { // This line would use `prefersJSON(acceptHeader)` after integration
		contentType = "application/json; charset=utf-8"
		errorResp := ErrorResponseJSON{
			Error: ErrorDetail{
				StatusCode: statusCode,
				Message:    statusText,
				Detail:     optionalDetail,
			},
		}
		var err error
		body, err = jsonMarshalFunc(errorResp)
		if err != nil {
			fields := logger.LogFields{"error": err}
			if log != nil { // Check logger is not nil before using
				log.Error("Failed to marshal JSON error response. Falling back to HTML.", fields)
			}
			prefersJSONCurrentLogic = false // Force HTML path
		}
	}

	if !prefersJSONCurrentLogic {
		contentType = "text/html; charset=utf-8"

		// statusText is defined at the top of SendDefaultErrorResponse
		// if statusText == "" { statusText = "Error" } // Already handled

		// Determine title, heading, and base message for HTML response
		var finalTitle, finalHeading, baseMessage string
		defaultMsgData, isKnownCode := defaultHTMLMessages[statusCode]

		if isKnownCode {
			finalTitle = defaultMsgData.Title
			finalHeading = defaultMsgData.Heading
			baseMessage = defaultMsgData.Message
		} else { // Generic error
			finalTitle = fmt.Sprintf("%d %s", statusCode, statusText)
			finalHeading = statusText
			baseMessage = "An unexpected error occurred."
		}

		// Construct the final HTML-safe message body string, incorporating optionalDetail
		htmlSafeMessageBody := func() string {
			if !isKnownCode { // Generic error
				if optionalDetail != "" {
					// For generic errors, if detail is present, it becomes the message (escaped)
					return html.EscapeString(optionalDetail)
				}
				// No optionalDetail for generic error:
				return baseMessage // which is "An unexpected error occurred." (safe plain text)
			}
			// Known error (isKnownCode == true)
			if optionalDetail != "" {
				// For known errors, if detail is present, it's appended to base message (escaped)
				return baseMessage + " " + html.EscapeString(optionalDetail)
			}
			// No optionalDetail for known error:
			return baseMessage // from map (safe plain text)
		}() // Call the func immediately

		body = GenerateHTMLResponseBodyForTest(finalTitle, finalHeading, htmlSafeMessageBody)
	}

	headers := []hpack.HeaderField{
		{Name: ":status", Value: fmt.Sprintf("%d", statusCode)},
		{Name: "content-type", Value: contentType},
		{Name: "content-length", Value: fmt.Sprintf("%d", len(body))},
		{Name: "cache-control", Value: "no-cache, no-store, must-revalidate"},
		{Name: "pragma", Value: "no-cache"},
		{Name: "expires", Value: "0"},
	}

	err := stream.SendHeaders(headers, len(body) == 0)
	if err != nil {
		if log != nil {
			log.Error("Error sending default error response headers for stream.", logger.LogFields{"error": err, "streamID": streamID(stream), "statusCode": statusCode})
		}
		return
	}

	if len(body) > 0 {
		_, err = stream.WriteData(body, true)
		if err != nil {
			if log != nil {
				log.Error("Error sending default error response body for stream.", logger.LogFields{"error": err, "streamID": streamID(stream), "statusCode": statusCode})
			}
		}
	}
}

// generateHTMLResponseBody creates a simple HTML error page.
func GenerateHTMLResponseBodyForTest(title, heading, message string) []byte {
	titleEsc := html.EscapeString(title)
	headingEsc := html.EscapeString(heading)
	// message is assumed to be already escaped or safe static text
	// If message comes from user input/detail, it should be escaped before calling this.
	// However, WriteErrorResponse does escape detailMessage before forming the final message string passed here.
	body := fmt.Sprintf(`<html><head><title>%s</title></head><body><h1>%s</h1><p>%s</p></body></html>`, titleEsc, headingEsc, message)
	return []byte(body)
}

// streamID tries to extract stream ID from ResponseWriter if it's a Stream.
// This is a helper for logging, actual type assertion might be needed.
func streamID(s http2.ResponseWriter) interface{} {
	if st, ok := s.(interface{ ID() uint32 }); ok {
		return st.ID()
	}
	return "unknown"
}

// TestingOnlySetJSONMarshal is used by tests to mock json.Marshal behavior.
// It sets the internal json.Marshal function and returns the original.
func TestingOnlySetJSONMarshal(fn func(v interface{}) ([]byte, error)) func(v interface{}) ([]byte, error) {
	original := jsonMarshalFunc
	jsonMarshalFunc = fn
	return original
}

// GetDefaultHTMLMessageInfo is used by tests to access default HTML message components.
// It returns the title and message for a given status code, and a boolean indicating if found.
func GetDefaultHTMLMessageInfo(statusCode int) (info struct {
	Title   string
	Heading string
	Message string
}, found bool) {
	info, found = defaultHTMLMessages[statusCode]
	return
}
