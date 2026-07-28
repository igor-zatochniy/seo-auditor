package main

import (
	"net/url"

	"github.com/igor-zatochniy/seo-auditor/internal/robots"
)

func robotsRequestPath(parsed *url.URL) string {
	return robots.RequestPath(parsed)
}

func isPathAllowedByRobots(content, userAgent, requestPath string) bool {
	return robots.IsPathAllowed(content, userAgent, requestPath)
}
