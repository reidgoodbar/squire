package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"squire/internal/catalog"
)

func main() {
	markdownOut := flag.String("markdown-out", "docs/commands.generated.md", "Path to write the generated Markdown command reference")
	websiteOut := flag.String("website-out", "", "Optional path to write the generated website command reference page")
	flag.Parse()

	if err := writeMarkdown(*markdownOut); err != nil {
		fatal(err)
	}
	if *websiteOut != "" {
		if err := writeWebsite(*websiteOut); err != nil {
			fatal(err)
		}
	}
}

func writeMarkdown(path string) error {
	body, err := catalog.RenderMarkdownReference()
	if err != nil {
		return err
	}
	return writeFile(path, body)
}

func writeWebsite(path string) error {
	body, err := catalog.RenderWebsiteCommandsPage()
	if err != nil {
		return err
	}
	return writeFile(path, body)
}

func writeFile(path, body string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(body), 0o644)
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
