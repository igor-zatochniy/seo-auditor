package main

import (
	"strings"
	"testing"
)

func TestRedactURLMasksSensitiveQueryValues(t *testing.T) {
	raw := "https://user:pass@example.com/reset?utm=1&token=secret&API_KEY=private&clientSecret=hidden&x-amz-security-token=aws&X-Amz-Signature=sig&X-Goog-Signature=goog&download_token_v2=download&temporarySignature=temp&mySecret=hidden&private_key=pk#access_token=fragment"
	got := redactURL(raw)
	want := "https://example.com/reset?API_KEY=%5BREDACTED%5D&X-Amz-Signature=%5BREDACTED%5D&X-Goog-Signature=%5BREDACTED%5D&clientSecret=%5BREDACTED%5D&download_token_v2=%5BREDACTED%5D&mySecret=%5BREDACTED%5D&private_key=%5BREDACTED%5D&temporarySignature=%5BREDACTED%5D&token=%5BREDACTED%5D&utm=%5BVALUE%5D&x-amz-security-token=%5BREDACTED%5D"

	if got != want {
		t.Fatalf("redactURL() = %q, want %q", got, want)
	}
	for _, leaked := range []string{
		"token=secret",
		"API_KEY=private",
		"clientSecret=hidden",
		"private_key=pk",
		"x-amz-security-token=aws",
		"X-Amz-Signature=sig",
		"X-Goog-Signature=goog",
		"download_token_v2=download",
		"temporarySignature=temp",
		"mySecret=hidden",
		"access_token=fragment",
		"utm=1",
	} {
		if strings.Contains(got, leaked) {
			t.Fatalf("redactURL leaked sensitive data: %q", got)
		}
	}
}

func TestRedactTextMasksURLsInsideErrorMessages(t *testing.T) {
	raw := `Get "https://example.com/api?token=secret&ok=1": dial tcp: timeout`
	got := redactText(raw)
	want := `Get "https://example.com/api?ok=%5BVALUE%5D&token=%5BREDACTED%5D": dial tcp: timeout`

	if got != want {
		t.Fatalf("redactText() = %q, want %q", got, want)
	}
	if strings.Contains(got, "token=secret") {
		t.Fatalf("redactText leaked sensitive data: %q", got)
	}
}

func TestSanitizeSEODataForStorageRedactsURLFieldsAndTruncatesError(t *testing.T) {
	data := SEOData{
		URL:          "https://example.com/page?token=secret&view=full",
		RedirectURL:  "https://example.com/callback?code=oauth-code",
		CanonicalURL: "/canonical?signature=private-signature",
		OGImage:      "https://cdn.example.com/image.png?sig=image-signature",
		ErrorMessage: "failed " + strings.Repeat("x", maxStoredErrorLength+100) + " https://example.com/reset?password=secret",
	}

	got := sanitizeSEODataForStorage(data)
	for field, value := range map[string]string{
		"url":           got.URL,
		"redirect_url":  got.RedirectURL,
		"canonical_url": got.CanonicalURL,
		"og_image":      got.OGImage,
		"error_message": got.ErrorMessage,
	} {
		if strings.Contains(value, "token=secret") ||
			strings.Contains(value, "code=oauth-code") ||
			strings.Contains(value, "signature=private-signature") ||
			strings.Contains(value, "sig=image-signature") ||
			strings.Contains(value, "view=full") {
			t.Fatalf("%s leaked sensitive data: %q", field, value)
		}
	}

	if len(got.ErrorMessage) > maxStoredErrorLength+3 {
		t.Fatalf("error_message was not truncated: length=%d", len(got.ErrorMessage))
	}
}
