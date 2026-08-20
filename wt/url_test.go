package wt

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestWebtransportURL(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "http is promoted",
			in:   "http://example.com",
			want: "https://example.com",
		},
		{
			name: "https passes through",
			in:   "https://example.com",
			want: "https://example.com",
		},
		{
			name: "nested path and trailing slash",
			in:   "http://example.com/one/two/",
			want: "https://example.com/one/two/",
		},
		{
			name: "existing query",
			in:   "https://example.com/wd/?mode=fast",
			want: "https://example.com/wd/?mode=fast",
		},
		{
			name: "encoded query value",
			in:   "https://example.com/wd/?next=https%3A%2F%2Fupstream.example%2Fa%2Fb",
			want: "https://example.com/wd/?next=https%3A%2F%2Fupstream.example%2Fa%2Fb",
		},
		{
			name: "literal URL in query",
			in:   "https://example.com/wd/?next=https://upstream.example/a/",
			want: "https://example.com/wd/?next=https://upstream.example/a/",
		},
		{
			name: "http query value is left alone",
			in:   "https://example.com/wd/?next=http://upstream.example/a/",
			want: "https://example.com/wd/?next=http://upstream.example/a/",
		},
		{
			name: "IPv6 host",
			in:   "http://[2001:db8::1]:8080/wd/",
			want: "https://[2001:db8::1]:8080/wd/",
		},
		{
			name: "fragment",
			in:   "https://example.com/wd/?x=1#next=https://fragment.example/",
			want: "https://example.com/wd/?x=1#next=https://fragment.example/",
		},
		{
			name: "uppercase scheme",
			in:   "HTTP://example.com/wd/",
			want: "https://example.com/wd/",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := webtransportURL(tc.in)
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

// TestWebtransportURLRejectsScheme pins the deliberate divergence from
// websocketURL, which passes unknown schemes through untouched. WebTransport
// has no cleartext or ws form, and failing here beats failing inside the dial.
func TestWebtransportURLRejectsScheme(t *testing.T) {
	for _, in := range []string{
		"ws://example.com/wd/",
		"wss://example.com/wd/",
		"ftp://example.com/wd/",
		"example.com/wd/", // no scheme at all
	} {
		t.Run(in, func(t *testing.T) {
			_, err := webtransportURL(in)
			require.Error(t, err)
			require.Contains(t, err.Error(), "webtransport requires an https URL")
		})
	}
}

func TestWebtransportURLRejectsUnparseable(t *testing.T) {
	_, err := webtransportURL("http://[::1")
	require.Error(t, err)
	require.Contains(t, err.Error(), "parse endpoint URL")
}
