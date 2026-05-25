package provider

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestParseJSONResponseUsesServerDateTTL(t *testing.T) {
	serverNow := time.Date(2026, 5, 25, 10, 0, 0, 0, time.UTC)
	localNow := time.Date(2026, 5, 25, 18, 0, 0, 0, time.UTC)
	body := []byte(fmt.Sprintf(`{"data":{"list":[{"ip":"203.0.113.10","port":40043,"expired":%d,"net":"联通"}]},"code":0,"message":"","status":200}`, serverNow.Add(10*time.Minute).UnixMilli()))

	up, err := ParseResponse(body, ParseOptions{
		DefaultScheme: "http",
		DefaultTTL:    30 * time.Minute,
		URLTTL:        3 * time.Minute,
		ServerNow:     serverNow,
		LocalNow:      localNow,
	})
	if err != nil {
		t.Fatal(err)
	}
	if up.Scheme != "http" || up.Host != "203.0.113.10" || up.Port != 40043 {
		t.Fatalf("unexpected upstream: %+v", up)
	}
	if up.Net != "联通" {
		t.Fatalf("unexpected net: %q", up.Net)
	}
	want := localNow.Add(10 * time.Minute)
	if !up.ExpiresAt.Equal(want) {
		t.Fatalf("expires = %s, want %s", up.ExpiresAt, want)
	}
}

func TestParseJSONResponseAcceptsStringPort(t *testing.T) {
	now := time.Date(2026, 5, 25, 10, 0, 0, 0, time.UTC)
	body := []byte(`{"data":{"list":[{"ip":"203.0.113.11","port":"40044","expired":0,"net":"移动"}]},"code":0,"message":"","status":200}`)

	up, err := ParseResponse(body, ParseOptions{
		DefaultScheme: "http",
		DefaultTTL:    10 * time.Minute,
		ServerNow:     now,
		LocalNow:      now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if up.Host != "203.0.113.11" || up.Port != 40044 {
		t.Fatalf("unexpected upstream: %+v", up)
	}
	if up.Net != "移动" {
		t.Fatalf("unexpected net: %q", up.Net)
	}
}

func TestParseJSONResponseUsesURLMinuteWhenExpiredMissing(t *testing.T) {
	now := time.Date(2026, 5, 25, 10, 0, 0, 0, time.UTC)
	body := []byte(`{"data":{"list":[{"ip":"203.0.113.12","port":40045,"net":"联通"}]},"code":0,"message":"","status":200}`)

	up, err := ParseResponse(body, ParseOptions{
		DefaultScheme: "http",
		DefaultTTL:    10 * time.Minute,
		URLTTL:        3 * time.Minute,
		ServerNow:     now,
		LocalNow:      now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !up.ExpiresAt.Equal(now.Add(3 * time.Minute)) {
		t.Fatalf("expires = %s, want %s", up.ExpiresAt, now.Add(3*time.Minute))
	}
}

func TestParsePlainResponseUsesDefaultTTL(t *testing.T) {
	now := time.Date(2026, 5, 25, 10, 0, 0, 0, time.UTC)
	up, err := ParseResponse([]byte("198.51.100.20:8080\n"), ParseOptions{
		DefaultScheme: "socks5",
		DefaultTTL:    5 * time.Minute,
		LocalNow:      now,
		ServerNow:     now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if up.Scheme != "socks5" || up.Host != "198.51.100.20" || up.Port != 8080 {
		t.Fatalf("unexpected upstream: %+v", up)
	}
	if !up.ExpiresAt.Equal(now.Add(5 * time.Minute)) {
		t.Fatalf("unexpected expiry: %s", up.ExpiresAt)
	}
}

func TestParsePlainResponseUsesURLMinuteBeforeDefaultTTL(t *testing.T) {
	now := time.Date(2026, 5, 25, 10, 0, 0, 0, time.UTC)
	up, err := ParseResponse([]byte("198.51.100.21:8081\n"), ParseOptions{
		DefaultScheme: "http",
		DefaultTTL:    10 * time.Minute,
		URLTTL:        3 * time.Minute,
		LocalNow:      now,
		ServerNow:     now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !up.ExpiresAt.Equal(now.Add(3 * time.Minute)) {
		t.Fatalf("expires = %s, want %s", up.ExpiresAt, now.Add(3*time.Minute))
	}
}

func TestNewFetcherKeepsURLMinuteTTLSeparately(t *testing.T) {
	fetcher := NewFetcher(Options{
		URL:        "https://service.ipzan.com/core-extract?num=1&minute=3&format=json",
		DefaultTTL: 10 * time.Minute,
	})

	if fetcher.defaultTTL != 10*time.Minute {
		t.Fatalf("defaultTTL = %s, want 10m", fetcher.defaultTTL)
	}
	if fetcher.urlTTL != 3*time.Minute {
		t.Fatalf("urlTTL = %s, want 3m", fetcher.urlTTL)
	}
}

func TestNewFetcherKeepsConfigTTLWithoutURLMinute(t *testing.T) {
	fetcher := NewFetcher(Options{
		URL:        "https://service.ipzan.com/core-extract?num=1&format=json",
		DefaultTTL: 10 * time.Minute,
	})

	if fetcher.defaultTTL != 10*time.Minute {
		t.Fatalf("defaultTTL = %s, want 10m", fetcher.defaultTTL)
	}
}

func TestParseResponseRejectsBadProviderData(t *testing.T) {
	_, err := ParseResponse([]byte(`{"data":{"list":[]},"code":0,"status":200}`), ParseOptions{
		DefaultScheme: "http",
		DefaultTTL:    time.Minute,
	})
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestSafeProviderErrorRedactsURL(t *testing.T) {
	raw := "https://service.ipzan.com/core-extract?no=abc&secret=def"
	err := errors.New(`Get "` + raw + `": context deadline exceeded`)
	got := safeProviderError(err, raw)
	if strings.Contains(got, "abc") || strings.Contains(got, "def") {
		t.Fatalf("provider error leaked secret: %s", got)
	}
}

func TestFetchRedactsRequestBuildError(t *testing.T) {
	fetcher := NewFetcher(Options{URL: "http://%zz?no=abc&secret=def"})
	_, err := fetcher.Fetch(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), "abc") || strings.Contains(err.Error(), "def") {
		t.Fatalf("request build error leaked secret: %s", err)
	}
}
