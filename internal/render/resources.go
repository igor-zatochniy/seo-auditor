package render

import "errors"

const MaxResourceIssues = 16

type ResourceIssue struct {
	URL          string `json:"url"`
	Type         string `json:"type"`
	Code         string `json:"code"`
	HTTP         int    `json:"http,omitempty"`
	URLTruncated bool   `json:"url_truncated,omitempty"`
}

type ResourceFailure struct {
	Code string
	HTTP int
}

func (e *ResourceFailure) Error() string { return e.Code }

func resourceIssue(out *Output, address, kind string, err error) {
	if len(out.Resources) >= MaxResourceIssues {
		out.ResourcesTruncated = true
		return
	}
	v := ResourceIssue{URL: address, Type: kind, Code: "resource_error"}
	var failure *ResourceFailure
	if errors.As(err, &failure) {
		v.Code, v.HTTP = failure.Code, failure.HTTP
	}
	if len(v.URL) > 2048 {
		v.URL = "[URL перевищує ліміт]"
		v.URLTruncated = true
	}
	out.Resources = append(out.Resources, v)
}
