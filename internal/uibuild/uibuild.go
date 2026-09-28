// Package uibuild assembles the standalone cpa-key-billing admin UI from its
// source parts. The assembly is deterministic so a release artifact can be
// rebuilt and compared byte for byte.
package uibuild

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// marker is the placeholder in ui.html that receives the assembled
// BILLING_MESSAGES catalog followed by the i18n runtime.
const marker = "// BILLING_I18N"

// Build reads ui.html, i18n.js, and every locale catalog below
// srcDir/locales, inlines the catalogs at the marker, and returns one
// self-contained HTML document.
//
// Determinism: locale languages are sorted before assembly and encoding/json
// orders map keys, so identical inputs always produce identical bytes.
func Build(srcDir string) ([]byte, error) {
	read := func(name string) ([]byte, error) {
		data, err := os.ReadFile(filepath.Join(srcDir, name))
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", name, err)
		}
		return data, nil
	}

	localeDir := filepath.Join(srcDir, "locales")
	localeFiles, err := os.ReadDir(localeDir)
	if err != nil {
		return nil, fmt.Errorf("read locales: %w", err)
	}
	var languages []string
	for _, entry := range localeFiles {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		languages = append(languages, strings.TrimSuffix(entry.Name(), ".json"))
	}
	sort.Strings(languages)
	if len(languages) == 0 {
		return nil, fmt.Errorf("no locale catalogs in %s", localeDir)
	}

	catalogs := make(map[string]map[string]string, len(languages))
	for _, language := range languages {
		raw, err := read(filepath.Join("locales", language+".json"))
		if err != nil {
			return nil, err
		}
		entries := map[string]string{}
		if err := json.Unmarshal(raw, &entries); err != nil {
			return nil, fmt.Errorf("parse locale %s: %w", language, err)
		}
		catalogs[language] = entries
	}
	data, err := json.Marshal(catalogs)
	if err != nil {
		return nil, fmt.Errorf("encode catalogs: %w", err)
	}

	i18n, err := read("i18n.js")
	if err != nil {
		return nil, err
	}
	script := append([]byte("const BILLING_MESSAGES = "), data...)
	script = append(script, ';', '\n')
	script = append(script, i18n...)

	ui, err := read("ui.html")
	if err != nil {
		return nil, err
	}
	if !bytes.Contains(ui, []byte(marker)) {
		return nil, fmt.Errorf("ui.html is missing the %q marker", marker)
	}
	return bytes.Replace(ui, []byte(marker), script, 1), nil
}
