package main

import "github.com/igor-zatochniy/seo-auditor/internal/robots"

func isPathAllowedByRobots(content, userAgent, requestPath string) bool {
	return robots.IsPathAllowed(content, userAgent, requestPath)
}
