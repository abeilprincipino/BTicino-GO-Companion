package entrypoint

import (
	"fmt"
	"sort"
	"strings"
)

const RTSPPathPrefix = "doorbell"

type StreamRoute struct {
	Path         string
	EntrypointID string
	DevAddr      string
}

func RTSPRoutes(entrypoints []Model) map[string]StreamRoute {
	routes := make(map[string]StreamRoute)
	for idx, ep := range entrypoints {
		if !ep.HasStream {
			continue
		}
		id := strings.TrimSpace(ep.ID)
		if id == "" {
			continue
		}
		token := sanitizePathToken(id)
		if token == "" {
			token = fmt.Sprintf("entrypoint%d", idx+1)
		}
		basePath := fmt.Sprintf("%s-%s", RTSPPathPrefix, token)
		path := basePath
		for n := 2; ; n++ {
			if _, exists := routes[path]; !exists {
				break
			}
			path = fmt.Sprintf("%s-%d", basePath, n)
		}
		routes[path] = StreamRoute{
			Path:         path,
			EntrypointID: id,
			DevAddr:      strings.TrimSpace(ep.DevAddr),
		}
	}
	return routes
}

func RTSPPathByEntrypointID(entrypoints []Model) map[string]string {
	routes := RTSPRoutes(entrypoints)
	out := make(map[string]string, len(routes))
	paths := make([]string, 0, len(routes))
	for path := range routes {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		route := routes[path]
		out[route.EntrypointID] = route.Path
	}
	return out
}

func sanitizePathToken(raw string) string {
	val := strings.ToLower(strings.TrimSpace(raw))
	if val == "" {
		return ""
	}

	var out strings.Builder
	lastDash := false
	for _, r := range val {
		switch {
		case r >= 'a' && r <= 'z':
			out.WriteRune(r)
			lastDash = false
		case r >= '0' && r <= '9':
			out.WriteRune(r)
			lastDash = false
		case r == '-' || r == '_':
			out.WriteRune(r)
			lastDash = false
		default:
			if !lastDash {
				out.WriteByte('-')
				lastDash = true
			}
		}
	}
	return strings.Trim(out.String(), "-")
}
