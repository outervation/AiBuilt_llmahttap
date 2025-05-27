package router

import (
	"fmt"
	"net/http"
	"sort"
	"strings"

	"golang.org/x/net/http2/hpack"

	"example.com/llmahttap/v2/internal/config"
	"example.com/llmahttap/v2/internal/http2"
	"example.com/llmahttap/v2/internal/logger"
	"example.com/llmahttap/v2/internal/server"
)

// Router holds the routing table and dispatches requests.
// It is responsible for matching incoming request paths against configured routes
// and forwarding the request to the appropriate handler.
type Router struct {
	// exactRoutes stores routes with MatchType "Exact".
	// The key is the PathPattern.
	exactRoutes map[string]config.Route

	// prefixRoutes stores routes with MatchType "Prefix".
	// These routes are sorted by PathPattern length in descending order
	// to ensure that the longest (most specific) prefix is matched first.
	prefixRoutes []config.Route

	handlerRegistry *server.HandlerRegistry
	mainLogger      *logger.Logger // Changed from errorLogger to mainLogger (*logger.Logger)
}

// NewRouter creates and initializes a new Router.
// It processes the routes from the configuration, sorts them for efficient matching,
// and stores them internally. It also requires a HandlerRegistry to create handlers
// and a Logger for logging.
// Errors during route validation (as per spec 1.2.3 and 1.2.4 ambiguity) should be
// handled by the config loader, so NewRouter assumes valid routes are passed.
func NewRouter(routes []config.Route, registry *server.HandlerRegistry, lg *logger.Logger) (*Router, error) { // Changed errorLogger to lg
	if registry == nil {
		return nil, fmt.Errorf("handler registry cannot be nil")
	}
	if lg == nil { // Changed from errorLogger
		return nil, fmt.Errorf("logger cannot be nil") // Changed message
	}

	exactMap := make(map[string]config.Route)
	var prefixList []config.Route

	for _, route := range routes {
		switch route.MatchType {
		case config.MatchTypeExact:
			exactMap[route.PathPattern] = route
		case config.MatchTypePrefix:
			prefixList = append(prefixList, route)
		}
	}

	// Sort prefix routes by path pattern length in descending order (longest first)
	// This is crucial for correct precedence (1.2.4: "longest (most specific) PathPattern MUST be chosen").
	sort.Slice(prefixList, func(i, j int) bool {
		return len(prefixList[i].PathPattern) > len(prefixList[j].PathPattern)
	})

	router := &Router{
		exactRoutes:     exactMap,
		prefixRoutes:    prefixList,
		handlerRegistry: registry,
		mainLogger:      lg, // Changed from errorLogger
	}
	return router, nil
}

// convertHpackHeadersToServerHeaders converts a slice of hpack.HeaderField
// to a slice of server.HeaderField.

// convertHpackHeadersToHttp2Headers converts []hpack.HeaderField to []http2.HeaderField.
func convertHpackHeadersToHttp2Headers(hpackHeaders []hpack.HeaderField) []http2.HeaderField {
	if hpackHeaders == nil {
		return nil
	}
	http2Headers := make([]http2.HeaderField, len(hpackHeaders))
	for i, hf := range hpackHeaders {
		http2Headers[i] = http2.HeaderField{Name: hf.Name, Value: hf.Value}
	}
	return http2Headers
}

// MatchedRouteInfo holds information about the matched route and the handler.
type MatchedRouteInfo struct {
	Handler       server.Handler // Changed from http2.Handler
	HandlerConfig config.Route   // Includes PathPattern, MatchType, HandlerType, and Opaque HandlerConfig
}

// Match finds a route and instantiates its handler based on the request path.
// It applies matching logic: exact match first, then longest prefix match.
// If a route is found, it uses the stored handlerRegistry to create the handler instance.
// Returns the matched route config, the instantiated handler, or an error.
// If no route matches, it returns (nil, nil, nil).
// If a route matches but handler creation fails, it returns (nil, nil, error).
func (r *Router) Match(path string) (matchedRoute *config.Route, handler server.Handler, err error) {
	// 1. Attempt Exact Match
	if routeConfig, ok := r.exactRoutes[path]; ok {
		h, e := r.handlerRegistry.CreateHandler(routeConfig.HandlerType, routeConfig.HandlerConfig.Bytes(), r.mainLogger)
		if e != nil {
			r.mainLogger.Error("Failed to create handler for exact match route", logger.LogFields{ // Use r.mainLogger
				"path":        path,
				"pattern":     routeConfig.PathPattern,
				"handlerType": routeConfig.HandlerType,
				"error":       e.Error(),
			})
			return nil, nil, fmt.Errorf("handler creation failed for path '%s' (route pattern '%s', type '%s'): %w", path, routeConfig.PathPattern, routeConfig.HandlerType, e)
		}
		// Return a pointer to a copy of the route config
		routeCopy := routeConfig
		return &routeCopy, h, nil
	}

	// 2. Attempt Prefix Match
	// r.prefixRoutes is already sorted by length (longest first).
	for _, routeConfig := range r.prefixRoutes {
		if strings.HasPrefix(path, routeConfig.PathPattern) {
			h, e := r.handlerRegistry.CreateHandler(routeConfig.HandlerType, routeConfig.HandlerConfig.Bytes(), r.mainLogger)
			if e != nil {
				r.mainLogger.Error("Failed to create handler for prefix match route", logger.LogFields{ // Use r.mainLogger
					"path":        path,
					"pattern":     routeConfig.PathPattern,
					"handlerType": routeConfig.HandlerType,
					"error":       e.Error(),
				})
				return nil, nil, fmt.Errorf("handler creation failed for path '%s' (route pattern '%s', type '%s'): %w", path, routeConfig.PathPattern, routeConfig.HandlerType, e)
			}
			// Return a pointer to a copy of the route config
			routeCopy := routeConfig
			return &routeCopy, h, nil
		}
	}

	// 3. No route matched
	return nil, nil, nil
}

// FindRoute matches the given request path against the configured routes.
// It follows the precedence rules:
// 1. Exact matches take precedence over prefix matches.
// 2. For prefix matches, the longest (most specific) pattern is chosen.
//
// If a route is found, it returns the instantiated handler and the route's configuration.
// If no route matches, it returns nil for both.
func (r *Router) FindRoute(path string) (*MatchedRouteInfo, error) {
	// First, check exact matches.
	if route, ok := r.exactRoutes[path]; ok {
		handler, err := r.handlerRegistry.CreateHandler(route.HandlerType, route.HandlerConfig.Bytes(), r.mainLogger)
		if err != nil {
			r.mainLogger.Error("Failed to create handler for exact route", logger.LogFields{
				"path":        path,
				"handlerType": route.HandlerType,
				"error":       err, // Note: err might not be a string here, consider err.Error() if logging structured fields.
			})
			return nil, fmt.Errorf("creating handler for '%s' (type %s): %w", path, route.HandlerType, err)
		}
		return &MatchedRouteInfo{Handler: handler, HandlerConfig: route}, nil
	}

	// Then, check prefix matches (sorted by longest prefix first).
	for _, route := range r.prefixRoutes {
		if strings.HasPrefix(path, route.PathPattern) {
			handler, err := r.handlerRegistry.CreateHandler(route.HandlerType, route.HandlerConfig.Bytes(), r.mainLogger)
			if err != nil {
				r.mainLogger.Error("Failed to create handler for prefix route", logger.LogFields{
					"path":        path,
					"pattern":     route.PathPattern,
					"handlerType": route.HandlerType,
					"error":       err, // Note: err might not be a string here, consider err.Error()
				})
				return nil, fmt.Errorf("creating handler for '%s' (pattern %s, type %s): %w", path, route.PathPattern, route.HandlerType, err)
			}
			return &MatchedRouteInfo{Handler: handler, HandlerConfig: route}, nil
		}
	}

	// No route matched.
	return nil, nil
}

// ServeHTTP dispatches the request to the appropriate handler based on the path.
// If no route matches, it sends a 404 Not Found response.
// If a handler is found but fails to be created, it sends a 500 Internal Server Error response.
// This method would be called by the HTTP/2 connection/server layer for each request stream.
func (r *Router) ServeHTTP(s server.ResponseWriterStream, req *http.Request) {
	r.mainLogger.Debug("Router.ServeHTTP: ENTERED", logger.LogFields{
		"method":   req.Method,
		"uri":      req.RequestURI,
		"path":     req.URL.Path,
		"streamID": s.ID(),
	})
	// Path is extracted from :path pseudo-header, typically available in req.URL.Path
	// For HTTP/2, the :path pseudo-header includes the query string.
	// The routing is based on the path component only.
	requestPath := req.URL.Path

	matchedInfo, err := r.FindRoute(requestPath)

	if err != nil { // Error during handler creation
		r.mainLogger.Error("Handler creation failed for request", logger.LogFields{ // Use r.mainLogger
			"path":   requestPath,
			"stream": s.ID(),
			"error":  err.Error(),
		})
		// Send 500 Internal Server Error
		// Pass req.Header as hpack.HeaderField for Accept header inspection
		var headers []http2.HeaderField
		for k, vv := range req.Header {
			for _, v := range vv {
				headers = append(headers, http2.HeaderField{Name: strings.ToLower(k), Value: v})
			}
		}
		// This uses the server's default error response mechanism.
		// Ensure the logger is not nil, though it should be guaranteed by NewRouter.
		server.WriteErrorResponse(s, http.StatusInternalServerError, headers, "Failed to initialize request handler.", r.mainLogger)
		return
	}

	if matchedInfo == nil { // No route matched
		r.mainLogger.Info("No route matched for request", logger.LogFields{ // Use r.mainLogger.Info
			"path":   requestPath,
			"stream": s.ID(),
		})
		var headers []http2.HeaderField
		for k, vv := range req.Header {
			for _, v := range vv {
				headers = append(headers, http2.HeaderField{Name: strings.ToLower(k), Value: v})
			}
		}
		server.WriteErrorResponse(s, http.StatusNotFound, headers, "The requested resource was not found.", r.mainLogger)
		return
	}

	// A handler was successfully found and created.
	// The HandlerConfig specific to this handler is available in matchedInfo.HandlerConfig.HandlerConfig
	// The http2.Handler interface's ServeHTTP2 method expects the stream and the request.
	// The handler itself should have received its config during creation via the factory.
	matchedInfo.Handler.ServeHTTP2(s, req) // s is already server.ResponseWriterStream
}
