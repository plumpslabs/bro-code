package version

import (
	"archive/zip"
	"bytes"
	"testing"
)

func TestSemverComparison(t *testing.T) {
	cases := []struct {
		latest  string
		current string
		want    bool
	}{
		{"v0.1.35", "v0.1.34", true},
		{"v0.2.0", "v0.1.35", true},
		{"v1.0.0", "v0.9.9", true},
		{"v0.1.34", "v0.1.34", false},
		{"v0.1.33", "v0.1.34", false},
		{"0.1.35", "0.1.34", true},
		{"v0.1.35-rc1", "v0.1.34", true},
	}

	for _, c := range cases {
		got := isNewerVersion(c.latest, c.current)
		if got != c.want {
			t.Errorf("isNewerVersion(%q, %q) = %v; want %v", c.latest, c.current, got, c.want)
		}
	}
}

func TestExtractFromZip(t *testing.T) {
	buf := new(bytes.Buffer)
	zw := zip.NewWriter(buf)

	f, err := zw.Create("brocode.exe")
	if err != nil {
		t.Fatal(err)
	}
	f.Write([]byte("mock-binary-data"))
	zw.Close()

	data, err := extractFromZip(buf.Bytes(), "brocode.exe")
	if err != nil {
		t.Fatalf("extractFromZip failed: %v", err)
	}
	if string(data) != "mock-binary-data" {
		t.Fatalf("expected extracted data 'mock-binary-data', got %q", string(data))
	}
}
