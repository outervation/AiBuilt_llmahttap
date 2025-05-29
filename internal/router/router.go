package router

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"runtime"
	"sort"
	"strings"

	"example.com/llmahttap/v2/internal/config"
	"example.com/llmahttap/v2/internal/http2"
	"example.com/llmahttap/v2/internal/logger"
	"example.com/llmahttap/v2/internal/server"
	"golang.org/x/net/http2/hpack"
)

// MatchedPathPatternKey is a context key for passing the matched PathPattern.
// It's an empty struct as it's only used as a key.
type MatchedPathPatternKey struct{}

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
	// Clone the request to avoid modifying the original request shared across streams/goroutines.
	// Only modify URL.Path for the handler's perspective.
	handlerReq := new(http.Request)
	*handlerReq = *req // Shallow copy is fine for most fields.
	// Deep copy URL as we'll modify Path.
	handlerReq.URL = new(url.URL)
	*handlerReq.URL = *req.URL // Shallow copy of URL struct members.

	matchedInfo, err := r.FindRoute(req.URL.Path)
	if err != nil { // Handler creation failed (e.g., bad regex in dynamic route config)
		r.mainLogger.Error("Router: Failed to process/create handler from route", logger.LogFields{"error": err, "path": req.URL.Path, "stream_id": s.ID()})
		server.SendDefaultErrorResponse(s, http.StatusInternalServerError, req, "Failed to initialize handler for the requested path.", r.mainLogger)
		return
	}

	if matchedInfo == nil || matchedInfo.Handler == nil { // No route matched
		r.mainLogger.Info("Router: No route matched", logger.LogFields{"path": req.URL.Path, "stream_id": s.ID()})
		server.SendDefaultErrorResponse(s, http.StatusNotFound, req, "The requested resource was not found on this server.", r.mainLogger)
		return
	}

	// Adjust req.URL.Path for the handler based on match type and pattern.
	route := matchedInfo.HandlerConfig
	originalReqPath := req.URL.Path // Use the original request path for logic here

	if route.MatchType == config.MatchTypePrefix {
		// For prefix match "/foo/", and request "/foo/bar.txt", handler gets "/bar.txt"
		// PathPattern for prefix matches is guaranteed to end with '/' by config validation.
		relativePath := strings.TrimPrefix(originalReqPath, route.PathPattern)
		handlerReq.URL.Path = "/" + relativePath // Ensure it's a rooted path for the handler.
		// Example: originalPath="/foo/", route.PathPattern="/foo/" -> relativePath="", handlerReq.URL.Path="/"
		// Example: originalPath="/foo/bar", route.PathPattern="/foo/" -> relativePath="bar", handlerReq.URL.Path="/bar"
		r.mainLogger.Debug("Router: Dispatching to handler with modified path for prefix match", logger.LogFields{
			"original_path": originalReqPath,
			"pattern":       route.PathPattern,
			"handler_path":  handlerReq.URL.Path,
			"stream_id":     s.ID(),
		})
	} else if route.MatchType == config.MatchTypeExact {
		// For exact match "/foo/file.txt", handler effectively gets "/file.txt"
		// For exact match "/file.txt", handler effectively gets "/file.txt"
		// For exact match "/", handler gets "/"
		// The StaticFileServer using this path will join it with its DocumentRoot.
		// e.g. DocumentRoot "/var/www", handler_path "/file.txt" -> serves "/var/www/file.txt"
		if route.PathPattern == "/" {
			handlerReq.URL.Path = "/"
		} else {
			// Use path.Base to get the last element of the pattern.
			// Prepend "/" to make it a rooted path for the handler.
			// This means if PathPattern is "/api/v1/data.json", handler_path becomes "/data.json".
			// If PathPattern is "/data.json", handler_path becomes "/data.json".
			handlerReq.URL.Path = "/" + path.Base(route.PathPattern)
		}
		r.mainLogger.Debug("Router: Dispatching to handler with modified path for exact match", logger.LogFields{
			"original_path": originalReqPath,
			"pattern":       route.PathPattern,
			"handler_path":  handlerReq.URL.Path,
			"stream_id":     s.ID(),
		})
	} else {
		// This case should ideally be prevented by config validation.
		r.mainLogger.Error("Router: Unknown or unsupported MatchType", logger.LogFields{
			"match_type":   route.MatchType,
			"path_pattern": route.PathPattern,
			"stream_id":    s.ID(),
		})
		server.SendDefaultErrorResponse(s, http.StatusInternalServerError, req, "Internal server error due to routing configuration.", r.mainLogger)
		return
	}

	// Store the matched route config in the request context for the handler to access if needed.
	ctxWithRoute := context.WithValue(handlerReq.Context(), MatchedPathPatternKey{}, route) // Using local MatchedPathPatternKey
	handlerReq = handlerReq.WithContext(ctxWithRoute)

	// Dispatch to the handler
	defer func() {
		if rcv := recover(); rcv != nil {
			stack := make([]byte, 4096) // 4KB for stack trace
			stack = stack[:runtime.Stack(stack, false)]
			r.mainLogger.Error("Handler panic recovered by router", logger.LogFields{
				"panic":        rcv,
				"stack":        string(stack),
				"request_uri":  req.RequestURI, // Log original request URI
				"method":       req.Method,
				"handler_path": handlerReq.URL.Path, // Log path seen by handler
				"stream_id":    s.ID(),
			})
			// Attempt to send a 500 error, but this might fail if headers are already sent.
			// ResponseWriterStream needs a way to check this, or SendDefaultErrorResponse handles it.
			server.SendDefaultErrorResponse(s, http.StatusInternalServerError, req, "The server encountered an internal error processing your request.", r.mainLogger)
		}
	}()
	r.mainLogger.Debug("Router: Dispatching to handler", logger.LogFields{
		"handler_type": route.HandlerType,
		"handler_path": handlerReq.URL.Path,
		"stream_id":    s.ID(),
	})
	matchedInfo.Handler.ServeHTTP2(s, handlerReq)
}
