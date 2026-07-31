package robots

import (
	"net/url"
	"testing"
)

func TestPercentEncodedUnreservedOctetsMatchRules(t *testing.T) {
	content := "User-agent: *\nDisallow: /private/data\n"

	for _, requestPath := range []string{
		"/private/data",
		"/private/%64%61%74%61",
		"/private/%64%61%74%61?view=%66ull",
	} {
		if IsPathAllowed(content, "ExampleBot/1.0", requestPath) {
			t.Fatalf("expected rule to disallow normalized path %q", requestPath)
		}
	}
}

func TestPercentEncodedRuleMatchesDecodedRequestPath(t *testing.T) {
	content := "User-agent: *\nDisallow: /private/%64%61%74%61\n"
	if IsPathAllowed(content, "ExampleBot/1.0", "/private/data") {
		t.Fatal("expected percent-encoded rule to disallow decoded request path")
	}
}

func TestReservedPercentEncodingRemainsLiteral(t *testing.T) {
	content := "User-agent: *\nDisallow: /path/file-with-a-%2a.html\n"

	if IsPathAllowed(content, "ExampleBot/1.0", "/path/file-with-a-%2A.html") {
		t.Fatal("expected equivalent reserved percent encoding to match")
	}
	if !IsPathAllowed(content, "ExampleBot/1.0", "/path/file-with-a-value.html") {
		t.Fatal("percent-encoded wildcard must remain a literal octet")
	}
}

func TestRequestPathNormalizesPathAndQueryOctets(t *testing.T) {
	target, err := url.Parse("https://example.com/private/%64%61%74%61?q=%76alue")
	if err != nil {
		t.Fatalf("parse target URL: %v", err)
	}

	if got, want := RequestPath(target), "/private/data?q=value"; got != want {
		t.Fatalf("RequestPath() = %q, want %q", got, want)
	}
}

func TestUTF8RuleMatchesPercentEncodedRequestPath(t *testing.T) {
	content := "User-agent: *\nDisallow: /資料\n"
	if IsPathAllowed(content, "ExampleBot/1.0", "/%E8%B3%87%E6%96%99") {
		t.Fatal("expected UTF-8 rule to match percent-encoded request path")
	}
}

func TestRuleSpecificityCountsPercentEncodedOctets(t *testing.T) {
	content := `
User-agent: *
Disallow: /foo/%62ar
Allow: /foo/bar
`
	if !IsPathAllowed(content, "ExampleBot/1.0", "/foo/bar") {
		t.Fatal("equivalent rules must have equal specificity so Allow wins")
	}
}

func TestCompiledPolicyReusesPreparedMatchers(t *testing.T) {
	policy, err := CompilePolicy(
		"User-agent: *\nDisallow: /private/*\nAllow: /private/public$\n",
		"ExampleBot/1.0",
	)
	if err != nil {
		t.Fatalf("CompilePolicy() error = %v", err)
	}
	if len(policy.rules) != 2 {
		t.Fatalf("compiled rules = %d, want 2", len(policy.rules))
	}

	firstMatcher := policy.rules[0].matcher
	for range 100 {
		if policy.Allows("/private/data") {
			t.Fatal("compiled policy unexpectedly allowed private path")
		}
		if !policy.Allows("/private/public") {
			t.Fatal("compiled policy unexpectedly blocked explicitly allowed path")
		}
	}
	if policy.rules[0].matcher != firstMatcher {
		t.Fatal("policy replaced a prepared matcher during path checks")
	}
}
