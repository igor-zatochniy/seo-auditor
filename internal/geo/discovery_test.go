package geo

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestDiscoverySelectsRelevantDeepPagesAndIntent(t *testing.T) {
	urls := []string{"https://example.com/blog/stripe-chargeback-protection", "https://example.com/solutions/payments/stripe-chargeback-protection", "https://example.com/contact", "https://example.com/blog/how-to-prevent-chargebacks", "https://example.com/solutions/chargeback-prevention"}
	selection, err := SelectDiscoveryURLs(t.Context(), []string{"best Stripe chargeback protection", "how to prevent chargebacks"}, urls, 2)
	if err != nil || selection.MatchedQueries != 2 || !reflect.DeepEqual(selection.URLs, []string{urls[1], urls[3]}) {
		t.Fatalf("selection=%+v err=%v", selection, err)
	}
	urls[0], urls[4] = urls[4], urls[0]
	again, err := SelectDiscoveryURLs(t.Context(), []string{"best Stripe chargeback protection", "how to prevent chargebacks"}, urls, 2)
	if err != nil || !reflect.DeepEqual(selection, again) {
		t.Fatal("Порядок sitemap змінив відбір", again, err)
	}
}

func TestDiscoveryRejectsSecretURLsAndUnrelatedCandidates(t *testing.T) {
	r, err := SelectDiscoveryURLs(t.Context(), []string{"chargebacks"}, []string{"https://example.com/chargebacks?token=secret", "https://user:secret@example.com/chargebacks", "https://example.com/about", "https://example.com/chargebacks", "https://example.com/chargebacks"}, 50)
	if err != nil || !reflect.DeepEqual(r.URLs, []string{"https://example.com/chargebacks"}) {
		t.Fatal(r, err)
	}
}

func TestDiscoveryUsesRareTermsAndPhrases(t *testing.T) {
	urls := []string{"https://example.com/payment-processing", "https://example.com/solutions/stripe-chargeback"}
	for i := range 50 {
		urls = append(urls, fmt.Sprintf("https://example.com/payments/%d", i))
	}
	r, err := SelectDiscoveryURLs(t.Context(), []string{"stripe chargeback payments"}, urls, 1)
	if err != nil || len(r.URLs) != 1 || r.URLs[0] != urls[1] {
		t.Fatal(r, err)
	}
	r, err = SelectDiscoveryURLs(t.Context(), []string{"payment processing"}, []string{urls[0], "https://example.com/processing-payment"}, 1)
	if err != nil || len(r.URLs) != 1 || r.URLs[0] != urls[0] {
		t.Fatal(r, err)
	}
}

func TestDiscoveryBoundsWorkAndHonorsCancellation(t *testing.T) {
	urls := make([]string, MaxDiscoveryURLs)
	for i := range urls {
		urls[i] = fmt.Sprintf("https://example.com/chargebacks/%d", i)
	}
	queries := make([]string, MaxQueries)
	for i := range queries {
		queries[i] = "chargebacks"
	}
	r, err := SelectDiscoveryURLs(t.Context(), queries, urls, 50)
	if err != nil || len(r.URLs) != 50 || !r.Limited || r.MatchedQueries >= len(queries) {
		t.Fatal(r, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err = SelectDiscoveryURLs(ctx, queries, urls, 50); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err = SelectDiscoveryURLs(t.Context(), queries, append(urls, "extra"), 50); err == nil {
		t.Fatal("Ліміт кандидатів не перевірено")
	}
	r, err = SelectDiscoveryURLs(t.Context(), []string{"chargebacks"}, []string{"https://example.com/" + strings.Repeat("chargebacks/", 40)}, 1)
	if err != nil || !r.Limited {
		t.Fatal(r, err)
	}
}
