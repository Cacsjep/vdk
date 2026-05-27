package rtspv2

import (
	"net/url"
	"strings"
	"testing"
)

// ----------------------------------------------------------------------------
// URL parsing.
// ----------------------------------------------------------------------------

// TestParseURL_DefaultPortPerScheme: RFC 7826 §4.2 defines the default ports
// as 554 for rtsp and 322 for rtsps. Unknown schemes are normalized to rtsp.
func TestParseURL_DefaultPortPerScheme(t *testing.T) {
	cases := []struct {
		name   string
		in     string
		host   string // expected pURL.Host after parseURL
		scheme string
	}{
		{name: "rtsp_no_port", in: "rtsp://user:pw@cam.local/path", host: "cam.local:554", scheme: "rtsp"},
		{name: "rtsps_no_port", in: "rtsps://user:pw@cam.local/path", host: "cam.local:322", scheme: "rtsps"},
		{name: "rtsp_explicit_port", in: "rtsp://user:pw@cam.local:9554/path", host: "cam.local:9554", scheme: "rtsp"},
		{name: "rtsps_explicit_port", in: "rtsps://user:pw@cam.local:9322/path", host: "cam.local:9322", scheme: "rtsps"},
		{name: "unknown_scheme_normalized", in: "http://cam.local/path", host: "cam.local:554", scheme: "rtsp"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &RTSPClient{headers: map[string]string{}}
			if err := c.parseURL(tc.in); err != nil {
				t.Fatalf("parseURL returned error: %v", err)
			}
			if c.pURL == nil {
				t.Fatal("pURL is nil after parseURL")
			}
			if got := c.pURL.Host; got != tc.host {
				t.Errorf("Host: got %q, want %q", got, tc.host)
			}
			if got := c.pURL.Scheme; got != tc.scheme {
				t.Errorf("Scheme: got %q, want %q", got, tc.scheme)
			}
		})
	}
}

// TestParseURL_AgreesWithStdlib cross-checks that parseURL's output is what
// net/url would build for the canonical RTSPS URL.
func TestParseURL_AgreesWithStdlib(t *testing.T) {
	c := &RTSPClient{headers: map[string]string{}}
	if err := c.parseURL("rtsps://user:pw@cam.local/some/path"); err != nil {
		t.Fatalf("parseURL error: %v", err)
	}
	u, err := url.Parse("rtsps://cam.local:322/some/path")
	if err != nil {
		t.Fatal(err)
	}
	if c.pURL.Host != u.Host {
		t.Errorf("host mismatch with net/url: got %q want %q", c.pURL.Host, u.Host)
	}
}

// ----------------------------------------------------------------------------
// Response header merging (WWW-Authenticate scheme priority).
// ----------------------------------------------------------------------------

// TestMergeResponseHeader_DigestPreferredOverBasic: per RFC 7235 §2.1 a client
// must select the strongest authentication scheme the server offers. Cameras
// that advertise both Digest and Basic must not have the session downgraded to
// Basic just because the Basic header arrived second.
func TestMergeResponseHeader_DigestPreferredOverBasic(t *testing.T) {
	const digest = `Digest realm="cam", nonce="abc123"`
	const basic = `Basic realm="cam"`

	cases := []struct {
		name  string
		order []struct{ key, value string }
	}{
		{name: "basic_then_digest", order: []struct{ key, value string }{
			{"WWW-Authenticate", basic}, {"WWW-Authenticate", digest},
		}},
		{name: "digest_then_basic", order: []struct{ key, value string }{
			{"WWW-Authenticate", digest}, {"WWW-Authenticate", basic},
		}},
		{name: "basic_digest_basic", order: []struct{ key, value string }{
			{"WWW-Authenticate", basic}, {"WWW-Authenticate", digest}, {"WWW-Authenticate", basic},
		}},
		{name: "digest_basic_digest", order: []struct{ key, value string }{
			{"WWW-Authenticate", digest}, {"WWW-Authenticate", basic}, {"WWW-Authenticate", digest},
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := map[string]string{}
			for _, h := range tc.order {
				mergeResponseHeader(res, h.key, h.value)
			}
			got, ok := res["WWW-Authenticate"]
			if !ok {
				t.Fatal("WWW-Authenticate missing from result map")
			}
			if !strings.Contains(got, "Digest") {
				t.Errorf("Digest got downgraded: final WWW-Authenticate = %q", got)
			}
			if strings.HasPrefix(strings.TrimSpace(got), "Basic ") {
				t.Errorf("final WWW-Authenticate starts with Basic: %q", got)
			}
		})
	}
}

// TestMergeResponseHeader_NonAuthHeadersOverwriteNormally guards against the
// auth-priority logic accidentally leaking into other header types.
func TestMergeResponseHeader_NonAuthHeadersOverwriteNormally(t *testing.T) {
	res := map[string]string{}
	mergeResponseHeader(res, "Content-Length", " 100")
	mergeResponseHeader(res, "Content-Length", " 200")
	if got := strings.TrimSpace(res["Content-Length"]); got != "200" {
		t.Errorf("non-auth header should overwrite: got %q want %q", got, "200")
	}
}

// TestMergeResponseHeader_SingleSchemePassthrough: a Digest-only or Basic-only
// response is preserved unchanged.
func TestMergeResponseHeader_SingleSchemePassthrough(t *testing.T) {
	for _, scheme := range []string{`Digest realm="x", nonce="y"`, `Basic realm="x"`} {
		res := map[string]string{}
		mergeResponseHeader(res, "WWW-Authenticate", scheme)
		if res["WWW-Authenticate"] != scheme {
			t.Errorf("single-scheme passthrough broken: got %q want %q",
				res["WWW-Authenticate"], scheme)
		}
	}
}
