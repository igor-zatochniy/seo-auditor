package seo

import (
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHTMLSizeThreshold(t *testing.T) {
	for _, size := range []int64{GooglebotHTMLRiskBytes - 1, GooglebotHTMLRiskBytes, GooglebotHTMLRiskBytes + 1} {
		body := "<body>" + strings.Repeat(" ", int(size)-13) + "</body>"
		resp := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/html; charset=utf-8"}}, Body: io.NopCloser(strings.NewReader(body))}
		data, err := ParsePage(resp, "https://example.com/", 8<<20, 5<<20)
		if err != nil || data.HTMLRawBytes == nil || *data.HTMLRawBytes != size || !data.HTMLSizeComplete {
			t.Fatalf("size=%d data=%+v err=%v", size, data, err)
		}
		want := "OK"
		if size >= GooglebotHTMLRiskBytes {
			want = "Googlebot cutoff risk"
		}
		if data.Googlebot2MBStatus != want {
			t.Fatalf("size=%d status=%s", size, data.Googlebot2MBStatus)
		}
	}
}

func TestHTMLSizeCountsDecompressedBytes(t *testing.T) {
	body := "<body>" + strings.Repeat(" x", 1<<20) + "</body>"
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Encoding", "gzip")
		z := gzip.NewWriter(w)
		_, _ = io.WriteString(z, body)
		_ = z.Close()
	}))
	defer s.Close()
	resp, err := s.Client().Get(s.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	d, err := ParsePage(resp, s.URL, 8<<20, 5<<20)
	if err != nil || d.HTMLRawBytes == nil || *d.HTMLRawBytes != int64(len(body)) || d.Googlebot2MBStatus != "Googlebot cutoff risk" {
		t.Fatalf("bytes=%v status=%s err=%v", d.HTMLRawBytes, d.Googlebot2MBStatus, err)
	}
}

func TestHTMLSizeDoesNotCallPartialReadSafe(t *testing.T) {
	resp := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/html; charset=utf-8"}}, Body: io.NopCloser(strings.NewReader("<body>" + strings.Repeat("x", 8192)))}
	d, err := ParsePage(resp, "https://example.com/", 4096, 4096)
	if err == nil || d.HTMLRawBytes == nil || d.HTMLSizeComplete || d.Googlebot2MBStatus != "Unknown (incomplete HTML)" {
		t.Fatalf("data=%+v err=%v", d, err)
	}
}

func TestHTMLSizeBeforeCharsetConversion(t *testing.T) {
	body := "<body>\xe9\xe9</body>"
	resp := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/html; charset=windows-1252"}}, Body: io.NopCloser(strings.NewReader(body))}
	d, err := ParsePage(resp, "https://example.com/", 4096, 4096)
	if err != nil || d.HTMLRawBytes == nil || *d.HTMLRawBytes != int64(len(body)) {
		t.Fatalf("data=%+v err=%v", d, err)
	}
}
