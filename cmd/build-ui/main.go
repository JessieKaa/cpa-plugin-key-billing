// Command build-ui assembles the standalone cpa-key-billing admin UI. The
// resulting HTML is self-contained and is published as a release asset; the
// plugin runtime no longer embeds it.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"cpa-key-billing/internal/uibuild"
)

func main() {
	out := flag.String("out", filepath.Join("dist", "cpa-key-billing-ui.html"), "output path for the assembled UI")
	src := flag.String("src", "", "UI source directory (default: internal/plugin next to the repository root)")
	flag.Parse()

	srcDir := *src
	if srcDir == "" {
		srcDir = filepath.Join(repoRoot(), "internal", "plugin")
	}
	outPath := *out
	if flag.NArg() > 0 {
		outPath = flag.Arg(0)
	}

	data, err := uibuild.Build(srcDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "build-ui:", err)
		os.Exit(1)
	}
	if dir := filepath.Dir(outPath); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			fmt.Fprintln(os.Stderr, "build-ui:", err)
			os.Exit(1)
		}
	}
	if err := os.WriteFile(outPath, data, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "build-ui:", err)
		os.Exit(1)
	}
	fmt.Printf("wrote %s (%d bytes)\n", outPath, len(data))
}

// repoRoot resolves the repository root from this source file's location so the
// default source directory works regardless of the working directory.
func repoRoot() string {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return "."
	}
	// <root>/cmd/build-ui/main.go
	return filepath.Dir(filepath.Dir(filepath.Dir(file)))
}
