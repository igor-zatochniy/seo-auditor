package crawler

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestURLShapeDoesNotRetainQuerySecrets(t *testing.T) {
	s := DescribeURL("https://example.com/catalog?token=private-secret&color=red&page=999#private")
	b, _ := json.Marshal(s)
	if strings.Contains(string(b), "private") || strings.Contains(string(b), "red") || s.Family != "https://example.com/catalog" || s.Page == nil || *s.Page != 999 {
		t.Fatalf("unsafe/inaccurate shape: %s", b)
	}
	if DescribeURL("https://user:password@example.com/").Complete {
		t.Fatal("credentials accepted")
	}
	if DescribeURL("https://example.com/?x=%zz").Complete {
		t.Fatal("malformed query accepted")
	}
	bucket := DescribeURL("https://example.com/?page=45673")
	raw, _ := json.Marshal(bucket)
	if bucket.Page == nil || *bucket.Page != 1000 || strings.Contains(string(raw), "45673") {
		t.Fatal("numeric query retained", string(raw))
	}
	oversize := DescribeURL("https://example.com/" + strings.Repeat("&", 1700) + "?sort=price")
	encoded, _ := json.Marshal(oversize)
	if len(encoded) > 4096 || oversize.Complete {
		t.Fatal("encoded shape exceeds storage budget")
	}
}
