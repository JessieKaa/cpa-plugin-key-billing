package uibuild

import (
	"bytes"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

func sourceDir(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot resolve test location")
	}
	// <root>/internal/uibuild/uibuild_test.go -> <root>/internal/plugin
	root := filepath.Dir(filepath.Dir(filepath.Dir(file)))
	return filepath.Join(root, "internal", "plugin")
}

func TestBuildIsDeterministic(t *testing.T) {
	dir := sourceDir(t)
	first, err := Build(dir)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Build(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("two builds produced different bytes")
	}
}

func TestBuildInlinesCatalogAndRuntime(t *testing.T) {
	out, err := Build(sourceDir(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"const BILLING_MESSAGES = ",
		`"zh-CN"`,
		"window.billingI18n",
	} {
		if !bytes.Contains(out, []byte(want)) {
			t.Fatalf("assembled UI is missing %q", want)
		}
	}
	if bytes.Contains(out, []byte("// BILLING_I18N")) {
		t.Fatal("assembled UI still contains the translation marker")
	}
}

// dataURI matches an inline data URI up to the closing double quote. Remote
// URLs that appear inside a data URI (for example the SVG XML namespace) are
// not network references and are allowed.
var dataURI = regexp.MustCompile(`data:[^"]*`)

// svgNamespace is an XML namespace identifier, not a fetchable resource.
var svgNamespace = []byte("http://www.w3.org/2000/svg")

func TestBuildHasNoRemoteReferences(t *testing.T) {
	out, err := Build(sourceDir(t))
	if err != nil {
		t.Fatal(err)
	}
	stripped := dataURI.ReplaceAll(out, []byte("data:"))
	stripped = bytes.ReplaceAll(stripped, svgNamespace, []byte("svg-namespace"))
	for _, scheme := range []string{"http://", "https://"} {
		if bytes.Contains(stripped, []byte(scheme)) {
			t.Fatalf("assembled UI references a remote URL with %s", scheme)
		}
	}
	for _, host := range []string{"cdn.jsdelivr.net", "unpkg.com", "cdnjs.cloudflare.com", "fonts.googleapis.com"} {
		if strings.Contains(string(out), host) {
			t.Fatalf("assembled UI references external host %s", host)
		}
	}
}
