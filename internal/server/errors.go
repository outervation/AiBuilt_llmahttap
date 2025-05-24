package server

import (
	"encoding/json"
	"fmt"
	"html"
	"math"
	"mime"
	"net/http"
	"strconv"
	"strings"

	"example.com/llmahttap/v2/internal/http2"
	"example.com/llmahttap/v2/internal/logger"

	"golang.org/x/net/http2/hpack"
	// golang.org/x/net/http2/hpack is not directly used by this file's logic
	// but keeping it in case http2.ResponseWriter implies its necessity elsewhere
	// or if SendDefaultErrorResponse might use it via stream object later.
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
func prefersJSON(acceptHeaderValue string) bool {
	if acceptHeaderValue == "" {
		return false // No Accept header, default to HTML
	}

	rawOfferStrings := strings.Split(acceptHeaderValue, ",")

	// Condition 1: "application/json is the first listed type with q > 0"
	// Check this first as it's based on the raw first part of the header.
	if len(rawOfferStrings) > 0 {
		firstRawOffer := strings.TrimSpace(rawOfferStrings[0])
		if firstRawOffer != "" {
			mt, ps, err := mime.ParseMediaType(firstRawOffer)
			if err == nil {
				qVal := 1.0
				if qStr, ok := ps["q"]; ok {
					qVal = parseQValue(qStr)
				}
				if strings.ToLower(mt) == "application/json" && qVal > 0.0 {
					return true
				}
			}
		}
	}

	// For conditions 2 and 3, parse all valid offers first.
	parsedOffers := make([]anOffer, 0, len(rawOfferStrings))
	for _, partStr := range rawOfferStrings {
		trimmedPart := strings.TrimSpace(partStr)
		if trimmedPart == "" {
			continue
		}

		mediaType, params, err := mime.ParseMediaType(trimmedPart)
		if err != nil {
			continue // Skip malformed parts
		}

		q := 1.0
		if qStr, ok := params["q"]; ok {
			q = parseQValue(qStr)
		}

		if q == 0.0 { // q=0 means "not acceptable", so skip this offer
			continue
		}
		parsedOffers = append(parsedOffers, anOffer{mediaType: strings.ToLower(mediaType), q: q})
	}

	if len(parsedOffers) == 0 { // All offers were malformed or had q=0
		return false
	}

	// Condition 3: "application/json is present with q=1.0"
	for _, offer := range parsedOffers {
		if offer.mediaType == "application/json" && offer.q == 1.0 {
			return true
		}
	}

	// Condition 2: "or */* is present and no other type has higher q-value than application/json"

	isWildcardStarStarPresent := false
	highestQForWildcardStarStar := 0.0
	for _, offer := range parsedOffers {
		if offer.mediaType == "*/*" { // implies offer.q > 0 due to earlier filter
			isWildcardStarStarPresent = true
			highestQForWildcardStarStar = math.Max(highestQForWildcardStarStar, offer.q)
		}
	}

	if !isWildcardStarStarPresent { // If "*/*" (with q>0) is not present, condition 2 cannot be met.
		return false
	}

	// Determine the effective q-value for "application/json" (Q_eff_json).
	// This considers explicit "application/json", then "application/*", then "*/*".
	qActualAppJson := 0.0
	foundActualAppJson := false
	qActualAppStar := 0.0
	foundActualAppStar := false

	for _, offer := range parsedOffers { // offers here all have q > 0
		if offer.mediaType == "application/json" {
			qActualAppJson = math.Max(qActualAppJson, offer.q)
			foundActualAppJson = true
		} else if offer.mediaType == "application/*" {
			qActualAppStar = math.Max(qActualAppStar, offer.q)
			foundActualAppStar = true
		}
	}

	var qEffJson float64
	if foundActualAppJson { // qActualAppJson will be >0 if found due to prior filtering
		qEffJson = qActualAppJson
	} else if foundActualAppStar { // qActualAppStar will be >0 if found
		qEffJson = qActualAppStar
	} else {
		// isWildcardStarStarPresent is true, so highestQForWildcardStarStar > 0.
		qEffJson = highestQForWildcardStarStar
	}

	// If, after all considerations, the effective q-value for application/json is 0
	// (e.g. all relevant offers had q=0, which were filtered, or no relevant offers found),
	// then it's not acceptable via this path.
	// This check is implicitly handled as qEffJson would be 0 if no matching q>0 offers were found.
	// And earlier, if all offers resulted in q=0, parsedOffers would be empty.
	// qEffJson here will be > 0 if we reached this point because:
	//   - foundActualAppJson => qActualAppJson > 0
	//   - OR foundActualAppStar => qActualAppStar > 0
	//   - OR highestQForWildcardStarStar > 0 (because isWildcardStarStarPresent is true)

	// Check if any *other* specific media type has a q-value strictly higher than qEffJson.
	// "Other" means not "application/json", not "application/*", not "*/*".
	for _, offer := range parsedOffers { // offers here all have q > 0
		if offer.mediaType != "application/json" &&
			offer.mediaType != "application/*" &&
			offer.mediaType != "*/*" {
			// This is a specific different type (e.g., "text/html", "image/png")
			if offer.q > qEffJson {
				return false // Another specific type is strictly more preferred.
			}
		}
	}

	// If we reach here, "*/*" was present (with q>0), and no other specific type
	// had a q-value strictly greater than the effective q-value of application/json.
	return true
}

// SendDefaultErrorResponse generates and sends a default HTTP error response on the given stream.
// It performs content negotiation based on the Accept header.
// stream: The HTTP/2 stream to send the response on.

// generateHTMLResponseBody creates a minimal HTML error page.
// title and heading are plain text (e.g., from http.StatusText or default messages) and will be HTML-escaped.
// htmlSafeMessage is a string that is already HTML-safe (e.g., pre-escaped dynamic content or static HTML-safe text).
func generateHTMLResponseBody(title, heading, htmlSafeMessage string) []byte {
	return []byte(fmt.Sprintf(
		"<html><head><title>%s</title></head><body><h1>%s</h1><p>%s</p></body></html>",
		html.EscapeString(title),
		html.EscapeString(heading),
		htmlSafeMessage, // Already HTML-safe
	))
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
		body, err = json.Marshal(errorResp)
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
			finalTitle = defaultMsgData.title
			finalHeading = defaultMsgData.heading
			baseMessage = defaultMsgData.message
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

		body = generateHTMLResponseBody(finalTitle, finalHeading, htmlSafeMessageBody)
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

// streamID tries to extract stream ID from ResponseWriter if it's a Stream.
// This is a helper for logging, actual type assertion might be needed.
func streamID(s http2.ResponseWriter) interface{} {
	if st, ok := s.(interface{ ID() uint32 }); ok {
		return st.ID()
	}
	return "unknown"
}
