package robots

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
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

func TestRobotsTXTIsImplicitlyAllowed(t *testing.T) {
	policy, err := CompilePolicy("User-agent: *\nDisallow: /\n", "ExampleBot/1.0")
	if err != nil {
		t.Fatalf("CompilePolicy() error = %v", err)
	}

	for _, requestPath := range []string{"/robots.txt", "/robots.txt?cache=bust", "/%72obots.txt"} {
		if !policy.Allows(requestPath) {
			t.Fatalf("robots.txt resource %q was not implicitly allowed", requestPath)
		}
	}
	if policy.Allows("/robots.txt/backup") {
		t.Fatal("implicit allowance unexpectedly covered a different path")
	}
}

func TestExplainURLContextReportsWinningDirectiveAndEqualSpecificityConflict(t *testing.T) {
	policy, err := CompilePolicy("User-agent: *\nDisallow: /private\nAllow: /private\n", "ExampleBot/1.0")
	if err != nil {
		t.Fatalf("CompilePolicy() error = %v", err)
	}
	target, _ := url.Parse("https://example.com/private/page")
	decision, err := policy.ExplainURLContext(context.Background(), target)
	if err != nil {
		t.Fatalf("ExplainURLContext() error = %v", err)
	}
	if !decision.Allowed || !decision.Conflict || decision.Directive != "Allow: /private" || decision.ConflictDirective != "Disallow: /private" {
		t.Fatalf("unexpected explanation: %+v", decision)
	}
}

func TestExplainURLContextReportsImplicitAndSpecialAllow(t *testing.T) {
	policy, err := CompilePolicy("User-agent: *\nDisallow: /\n", "ExampleBot/1.0")
	if err != nil {
		t.Fatalf("CompilePolicy() error = %v", err)
	}
	for raw, want := range map[string]string{
		"https://example.com/open":       "Disallow: /",
		"https://example.com/robots.txt": "implicit allow: robots.txt is always accessible",
	} {
		target, _ := url.Parse(raw)
		decision, err := policy.ExplainURLContext(context.Background(), target)
		if err != nil || decision.Directive != want {
			t.Errorf("ExplainURLContext(%q) = %+v, %v; want directive %q", raw, decision, err, want)
		}
	}
}

func TestCompilePoliciesContextBoundsOptionalTokensAndHonorsProductToken(t *testing.T) {
	content := "User-agent: Google-Extended\nDisallow: /one\nDisallow: /two\nUser-agent: *\nDisallow: /private\n"
	policies, policyErrors, err := CompilePoliciesContext(context.Background(), content, []string{"Google-Extended", "OAI-SearchBot"}, 1, 1<<20)
	if err != nil {
		t.Fatalf("CompilePoliciesContext() error = %v", err)
	}
	if policyErrors["Google-Extended"] == nil {
		t.Fatal("oversized selected product-token group did not become a per-token unknown")
	}
	if policies["Google-Extended"] != nil {
		t.Fatal("policy with too many rules was compiled")
	}
	if policies["OAI-SearchBot"] == nil || policies["OAI-SearchBot"].Allows("/private") {
		t.Fatal("a bounded product-token error prevented an unrelated wildcard policy")
	}
}

func TestCompilePoliciesContextSharesEquivalentPoliciesAndEnforcesCombinedWeight(t *testing.T) {
	content := "User-agent: *\nDisallow: /private\n"
	policies, _, err := CompilePoliciesContext(context.Background(), content, []string{"Googlebot", "OAI-SearchBot"}, 10, 1<<20)
	if err != nil {
		t.Fatalf("CompilePoliciesContext() error = %v", err)
	}
	if policies["Googlebot"] != policies["OAI-SearchBot"] {
		t.Fatal("identical selected groups were not shared")
	}
	policies, failures, err := CompilePoliciesContext(context.Background(), content, []string{"Googlebot", "OAI-SearchBot"}, 10, 1)
	if err != nil {
		t.Fatalf("bounded optional compilation should not fail the main parser: %v", err)
	}
	if len(policies) != 0 || len(failures) != 2 {
		t.Fatalf("combined budget was not enforced: policies=%d failures=%v", len(policies), failures)
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

func TestPartialProductTokenFallsBackToWildcardGroup(t *testing.T) {
	content := `
User-agent: SEOParser
Allow: /

User-agent: *
Disallow: /
`
	if IsPathAllowed(content, "Go-SEOParser-Bot/1.0", "/page") {
		t.Fatal("partial product token unexpectedly matched a specific robots.txt group")
	}
}

func TestApplebotUsesGooglebotRulesBeforeWildcard(t *testing.T) {
	content := "User-agent: Googlebot\nDisallow: /private\nUser-agent: *\nAllow: /\n"
	for _, compile := range []func(string, string) (*Policy, error){CompilePolicy, func(content, agent string) (*Policy, error) {
		policies, _, err := CompilePoliciesContext(context.Background(), content, []string{agent}, 100, 1<<20)
		return policies[agent], err
	}} {
		policy, err := compile(content, "Applebot")
		if err != nil || policy == nil || policy.Allows("/private/page") {
			t.Fatalf("Applebot must inherit Googlebot's Disallow, policy=%v error=%v", policy, err)
		}
		if !policy.Allows("/public") {
			t.Fatal("Googlebot fallback unexpectedly blocked a public path")
		}
	}
	if !IsPathAllowed(content, "OtherBot", "/private/page") {
		t.Fatal("Applebot fallback changed the wildcard group for other agents")
	}
}

func TestApplebotSpecificRulesAndWildcardRemainDistinct(t *testing.T) {
	content := "User-agent: Applebot\nAllow: /private\nUser-agent: Googlebot\nDisallow: /private\nUser-agent: *\nDisallow: /other\n"
	if !IsPathAllowed(content, "Applebot/0.1", "/private/page") || IsPathAllowed(content, "Googlebot/2.1", "/private/page") {
		t.Fatal("provider-specific Applebot rules did not take precedence")
	}
	if !IsPathAllowed("User-agent: *\nAllow: /private\n", "Applebot", "/private/page") {
		t.Fatal("Applebot did not fall back to wildcard without provider rules")
	}
	if !IsPathAllowed("User-agent: Applebot-Extended\nDisallow: /private\nUser-agent: *\nAllow: /\n", "Applebot", "/private/page") {
		t.Fatal("Applebot-Extended policy must not apply to Applebot crawler")
	}
}

func TestSupportedRobotsLineEndingsHaveIdenticalVerdicts(t *testing.T) {
	tests := []struct {
		name    string
		content string
	}{
		{name: "LF", content: "User-agent: *\nDisallow: /private\nAllow: /private/open\n"},
		{name: "CRLF", content: "User-agent: *\r\nDisallow: /private\r\nAllow: /private/open\r\n"},
		{name: "CR", content: "User-agent: *\rDisallow: /private\rAllow: /private/open\r"},
		{name: "mixed", content: "User-agent: *\rDisallow: /private\nAllow: /private/open\r\n"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if IsPathAllowed(tt.content, "ExampleBot/1.0", "/private/data") {
				t.Fatal("valid robots.txt line endings lost the Disallow rule")
			}
			if !IsPathAllowed(tt.content, "ExampleBot/1.0", "/private/open") {
				t.Fatal("valid robots.txt line endings lost the more specific Allow rule")
			}
		})
	}
}

func TestCompiledPolicyReusesPreparedPatterns(t *testing.T) {
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

	firstPattern := policy.rules[0].pattern
	for range 100 {
		if policy.Allows("/private/data") {
			t.Fatal("compiled policy unexpectedly allowed private path")
		}
		if !policy.Allows("/private/public") {
			t.Fatal("compiled policy unexpectedly blocked explicitly allowed path")
		}
	}
	if policy.rules[0].pattern != firstPattern {
		t.Fatal("policy replaced a prepared pattern during path checks")
	}
}

func TestPreparedWildcardPatternPreservesAnchoredMatching(t *testing.T) {
	policy, err := CompilePolicy(
		"User-agent: *\nDisallow: /files/*report$\n",
		"ExampleBot/1.0",
	)
	if err != nil {
		t.Fatalf("CompilePolicy() error = %v", err)
	}
	if policy.Allows("/files/report-draft-report") {
		t.Fatal("anchored wildcard pattern did not match its final occurrence")
	}
	if !policy.Allows("/files/report-draft-report/appendix") {
		t.Fatal("anchored wildcard pattern matched a path with a trailing suffix")
	}
}

func TestCompilePolicyContextAcceptsMoreThan1024Rules(t *testing.T) {
	var content strings.Builder
	content.WriteString("User-agent: *\n")
	for index := range 2048 {
		content.WriteString("Disallow: /private/")
		content.WriteString(fmt.Sprintf("%d\n", index))
	}

	policy, err := CompilePolicyContext(
		context.Background(),
		content.String(),
		"ExampleBot/1.0",
	)
	if err != nil {
		t.Fatalf("CompilePolicyContext() rejected a byte-bounded policy: %v", err)
	}
	if policy.Allows("/private/2047") {
		t.Fatal("policy lost a rule after the former 1024-rule boundary")
	}
}

func TestCompilePolicyContextRejectsContentAboveByteLimit(t *testing.T) {
	content := strings.Repeat("x", MaxPolicyBytes+1)

	policy, err := CompilePolicyContext(context.Background(), content, "ExampleBot/1.0")
	if err == nil {
		t.Fatal("CompilePolicyContext() unexpectedly accepted content above the byte limit")
	}
	if policy != nil {
		t.Fatal("CompilePolicyContext() returned a policy for oversized content")
	}
}

func TestCompilePolicyContextHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	policy, err := CompilePolicyContext(
		ctx,
		"User-agent: *\nDisallow: /private\n",
		"ExampleBot/1.0",
	)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("CompilePolicyContext() error = %v, want context.Canceled", err)
	}
	if policy != nil {
		t.Fatal("CompilePolicyContext() returned a policy after cancellation")
	}
}

func TestPolicyAllowsContextHonorsCancellation(t *testing.T) {
	policy, err := CompilePolicy("User-agent: *\nDisallow: /private\n", "ExampleBot/1.0")
	if err != nil {
		t.Fatalf("CompilePolicy() error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	allowed, err := policy.AllowsContext(ctx, "/private/page")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("AllowsContext() error = %v, want context.Canceled", err)
	}
	if allowed {
		t.Fatal("AllowsContext() allowed a path after cancellation")
	}
}

func TestPolicyAllowsContextEnforcesComplexityBudget(t *testing.T) {
	rules := make([]compiledRule, 2_000)
	pattern := "/" + strings.Repeat("a", 64) + "*missing"
	for index := range rules {
		rules[index] = compiledRule{pattern: pattern, specificity: len(pattern)}
	}
	policy := &Policy{rules: rules}

	allowed, err := policy.AllowsContext(
		context.Background(),
		"/"+strings.Repeat("a", 2_047),
	)
	if !errors.Is(err, ErrPolicyMatchComplexity) {
		t.Fatalf("AllowsContext() error = %v, want ErrPolicyMatchComplexity", err)
	}
	if allowed {
		t.Fatal("complex policy unexpectedly allowed the path")
	}
}

func BenchmarkPolicyAllowsComplexityLimit(b *testing.B) {
	const commonPrefixLength = 64
	var content strings.Builder
	content.Grow(MaxPolicyBytes)
	content.WriteString("User-agent: *\n")
	commonPrefix := strings.Repeat("a", commonPrefixLength)
	for index := 0; ; index++ {
		rule := fmt.Sprintf("Disallow: /%s*missing-%d$\n", commonPrefix, index)
		if content.Len()+len(rule) > MaxPolicyBytes {
			break
		}
		content.WriteString(rule)
	}

	policy, err := CompilePolicy(content.String(), "ExampleBot/1.0")
	if err != nil {
		b.Fatalf("compile maximum-size policy: %v", err)
	}
	requestPath := "/" + strings.Repeat("a", 2047)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		allowed, err := policy.AllowsContext(context.Background(), requestPath)
		if !errors.Is(err, ErrPolicyMatchComplexity) {
			b.Fatalf("policy match error = %v, want ErrPolicyMatchComplexity", err)
		}
		if allowed {
			b.Fatal("complex benchmark policy unexpectedly allowed the path")
		}
	}
}
