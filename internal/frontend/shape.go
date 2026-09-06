package frontend

import (
	"context"
	"slices"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Every result revision 2026-07-28 defines carries a completion type, and every
// cacheable one carries a lifetime and a scope.
const (
	// resultTypeComplete is the only result type the gateway emits. The
	// input_required flow belongs to Multi Round-Trip Requests, which this
	// gateway does not implement, so a client never has to handle one.
	resultTypeComplete = "complete"
	// cacheScopePrivate is the only cache scope the gateway emits. The surface
	// depends on the subject and on the x-mcp-config header, so "public" would
	// let an intermediary serve one subject's tools to another.
	cacheScopePrivate = "private"
	// defaultTTLMs is the cache lifetime of a result whose backend offered
	// none.
	defaultTTLMs = 60_000
)

// shapeResults is receiving middleware that brings every result into the shape
// revision 2026-07-28 requires, whatever produced it.
//
// It is not decoration over what the SDK already does. The SDK's own discover
// handler defaults `cacheScope` to "public", which this gateway must never
// emit, and it fills in the completion type only for requests that took the
// per-request protocol path.
func shapeResults(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		res, err := next(ctx, method, req)
		if err != nil {
			return nil, err
		}
		shape(res)
		return res, nil
	}
}

// shape rewrites the fields of one result in place.
func shape(res mcp.Result) {
	switch r := res.(type) {
	case *mcp.DiscoverResult:
		// The gateway speaks one revision. The SDK would otherwise advertise
		// every revision it knows, including the legacy ones.
		r.SupportedVersions = slices.Clone(SupportedVersions)
		r.ResultType = resultTypeComplete
		r.Cacheable = cacheable(r.Cacheable)
	case *mcp.ListToolsResult:
		r.ResultType = resultTypeComplete
		r.Cacheable = cacheable(r.Cacheable)
	case *mcp.ListPromptsResult:
		r.ResultType = resultTypeComplete
		r.Cacheable = cacheable(r.Cacheable)
	case *mcp.ListResourcesResult:
		r.ResultType = resultTypeComplete
		r.Cacheable = cacheable(r.Cacheable)
	case *mcp.ListResourceTemplatesResult:
		r.ResultType = resultTypeComplete
		r.Cacheable = cacheable(r.Cacheable)
	case *mcp.ReadResourceResult:
		// The SDK keeps this one's completion type unexported and sets it
		// itself; only the caching hints are ours to supply.
		r.Cacheable = cacheable(r.Cacheable)
	}
}

// cacheable supplies the caching hints a result is missing, and forces the
// scope even when it has one.
func cacheable(c mcp.Cacheable) mcp.Cacheable {
	if c.TTLMs <= 0 {
		c.TTLMs = defaultTTLMs
	}
	c.CacheScope = cacheScopePrivate
	return c
}
