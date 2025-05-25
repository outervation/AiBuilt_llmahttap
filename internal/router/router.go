package router

import (
	"fmt"
	"net/http"
	"sort"
	"strings"

	NetHPack "golang.org/x/net/http2/hpack" // Alias for debugging

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
	errorLogger     *logger.ErrorLogger // For logging router-specific errors, e.g., handler creation failure
}

// NewRouter creates and initializes a new Router.
// It processes the routes from the configuration, sorts them for efficient matching,
// and stores them internally. It also requires a HandlerRegistry to create handlers
// and an ErrorLogger for logging.
// Errors during route validation (as per spec 1.2.3 and 1.2.4 ambiguity) should be
// handled by the config loader, so NewRouter assumes valid routes are passed.
func NewRouter(routes []config.Route, registry *server.HandlerRegistry, errorLogger *logger.ErrorLogger) (*Router, error) {
	if registry == nil {
		return nil, fmt.Errorf("handler registry cannot be nil")
	}
	if errorLogger == nil {
		return nil, fmt.Errorf("error logger cannot be nil")
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
		errorLogger:     errorLogger,
	}
	return router, nil
}

// MatchedRouteInfo holds information about the matched route and the handler.
type MatchedRouteInfo struct {
	Handler       http2.Handler
	HandlerConfig config.Route // Includes PathPattern, MatchType, HandlerType, and Opaque HandlerConfig
}

// FindRoute matches the given request path against the configured routes.
// It follows the precedence rules:
// 1. Exact matches take precedence over prefix matches.
// 2. For prefix matches, the longest (most specific) pattern is chosen.
//
// If a route is found, it returns the instantiated handler and the route's configuration.
// If no route matches, it returns nil for both.
func (r *Router) FindRoute(path string) (*MatchedRouteInfo, error) {
	// 1. Attempt Exact Match (Spec 1.2.4: "An Exact match MUST take precedence over a Prefix match.")
	if route, ok := r.exactRoutes[path]; ok {
		handler, err := r.handlerRegistry.CreateHandler(route.HandlerType, route.HandlerConfig, r.errorLogger)
		if err != nil {
			// Log the error, as handler creation failure is a server-side issue.
			r.errorLogger.Error("Failed to create handler for exact match route", logger.LogFields{
				"path":        path,
				"handlerType": route.HandlerType,
				"error":       err.Error(),
			})
			// This error will likely lead to a 500 response upstream.
			return nil, err
		}
		return &MatchedRouteInfo{Handler: handler, HandlerConfig: route}, nil
	}

	// 2. Attempt Prefix Match (Spec 1.2.4: "If multiple Prefix matches are possible,
	//    the longest (most specific) PathPattern MUST be chosen.")
	// The prefixRoutes slice is already sorted by length (longest first).
	for _, route := range r.prefixRoutes {
		if strings.HasPrefix(path, route.PathPattern) {
			// Ensure prefix match logic is correct: "/static/" should match "/static/foo.txt"
			// but also "/static/" itself.
			handler, err := r.handlerRegistry.CreateHandler(route.HandlerType, route.HandlerConfig, r.errorLogger)
			if err != nil {
				r.errorLogger.Error("Failed to create handler for prefix match route", logger.LogFields{
					"path":        path,
					"pattern":     route.PathPattern,
					"handlerType": route.HandlerType,
					"error":       err.Error(),
				})
				return nil, err
			}
			return &MatchedRouteInfo{Handler: handler, HandlerConfig: route}, nil
		}
	}

	// 3. No route matched
	return nil, nil
}

// ServeHTTP dispatches the request to the appropriate handler based on the path.
// If no route matches, it sends a 404 Not Found response.
// If a handler is found but fails to be created, it sends a 500 Internal Server Error response.
// This method would be called by the HTTP/2 connection/server layer for each request stream.
func (r *Router) ServeHTTP(s *http2.Stream, req *http.Request) {
	// Path is extracted from :path pseudo-header, typically available in req.URL.Path
	// For HTTP/2, the :path pseudo-header includes the query string.
	// The routing is based on the path component only.
	requestPath := req.URL.Path

	matchedInfo, err := r.FindRoute(requestPath)

	if err != nil { // Error during handler creation
		r.errorLogger.Error("Handler creation failed for request", logger.LogFields{
			"path":   requestPath,
			"stream": s.ID(),
			"error":  err.Error(),
		})
		// Send 500 Internal Server Error
		// Pass req.Header as hpack.HeaderField for Accept header inspection
		var headers []NetHPack.HeaderField
		for k, vv := range req.Header {
			for _, v := range vv {
				headers = append(headers, NetHPack.HeaderField{Name: strings.ToLower(k), Value: v})
			}
		}
		// This uses the server's default error response mechanism.
		// Ensure the error logger is not nil, though it should be guaranteed by NewRouter.
		server.WriteErrorResponse(s, http.StatusInternalServerError, headers, "Failed to initialize request handler.")
		return
	}

	if matchedInfo == nil { // No route matched
		r.errorLogger.Info("No route matched for request", logger.LogFields{ // INFO level for 404s is common
			"path":   requestPath,
			"stream": s.ID(),
		})
		var headers []NetHPack.HeaderField
		for k, vv := range req.Header {
			for _, v := range vv {
				headers = append(headers, NetHPack.HeaderField{Name: strings.ToLower(k), Value: v})
			}
		}
		server.WriteErrorResponse(s, http.StatusNotFound, headers, "The requested resource was not found.")
		return
	}

	// A handler was successfully found and created.
	// The HandlerConfig specific to this handler is available in matchedInfo.HandlerConfig.HandlerConfig
	// The http2.Handler interface's ServeHTTP2 method expects the stream and the request.
	// The handler itself should have received its config during creation via the factory.
	matchedInfo.Handler.ServeHTTP2(s, req)
}
