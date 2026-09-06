// Command spec refreshes flyio/machines/fly-machines.openapi3.json from
// Fly's published Machines API document and applies the one patch the
// generated client still needs. Run it from the repository root, then
// `go generate ./...` to regenerate the client and docs:
//
//	go run ./hack/spec && go generate ./...
//
// It is deliberately not a go:generate step: `go generate ./...` has to stay
// reproducible offline, and CI diffs its output.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
)

// specURL is where Fly publishes the OpenAPI 3 document for the Machines
// API. It used to be a Swagger 2.0 document at /swagger/doc.json, which now
// redirects here.
const specURL = "https://docs.machines.dev/openapi.json"

const specPath = "flyio/machines/fly-machines.openapi3.json"

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "spec:", err)
		os.Exit(1)
	}
}

func run(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("spec", flag.ContinueOnError)
	src := fs.String("from", specURL, "URL or local path of the upstream document")
	out := fs.String("out", specPath, "where to write the patched document")
	if err := fs.Parse(args); err != nil {
		return err
	}

	raw, err := fetch(*src)
	if err != nil {
		return err
	}
	patched, report, err := patch(raw)
	if err != nil {
		return err
	}
	if err := os.WriteFile(*out, patched, 0o644); err != nil {
		return err
	}
	for _, line := range report {
		_, _ = fmt.Fprintln(stdout, line)
	}
	_, _ = fmt.Fprintf(stdout, "wrote %s (%d bytes)\n", *out, len(patched))
	return nil
}

func fetch(src string) ([]byte, error) {
	if _, err := os.Stat(src); err == nil {
		return os.ReadFile(src)
	}
	resp, err := http.Get(src) //nolint:gosec // the source is an operator-supplied flag
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch %s: %s", src, resp.Status)
	}
	return io.ReadAll(resp.Body)
}

// patch applies the corrections the generated client needs on top of the
// upstream document and re-encodes it with sorted keys and two-space
// indentation, so two runs against the same upstream produce the same
// bytes. It returns one report line per change made.
//
// Numbers are decoded as json.Number so that re-encoding does not rewrite
// them (a `1.0` would otherwise come back as `1`).
func patch(raw []byte) ([]byte, []string, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var doc map[string]any
	if err := dec.Decode(&doc); err != nil {
		return nil, nil, fmt.Errorf("decode upstream document: %w", err)
	}
	version, _ := doc["openapi"].(string)
	if version == "" {
		return nil, nil, errors.New("upstream document carries no openapi version; a Swagger 2.0 document needs converting first")
	}

	report := dropCaseDuplicateProperties(doc)

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(doc); err != nil {
		return nil, nil, err
	}
	return buf.Bytes(), report, nil
}

// dropCaseDuplicateProperties removes a property whose name differs from a
// sibling's only by case, keeping the lowercase one. Fly's document declares
// both `Version` and `version` on its secrets responses; the wire carries
// `version`, and oapi-codegen would generate two fields named Version and
// fail to compile.
func dropCaseDuplicateProperties(doc map[string]any) []string {
	var report []string
	components, _ := doc["components"].(map[string]any)
	schemas, _ := components["schemas"].(map[string]any)
	names := make([]string, 0, len(schemas))
	for name := range schemas {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		schema, _ := schemas[name].(map[string]any)
		props, _ := schema["properties"].(map[string]any)
		for _, dup := range caseDuplicates(props) {
			delete(props, dup)
			report = append(report, fmt.Sprintf("%s: dropped property %q (case-duplicate)", name, dup))
		}
	}
	return report
}

// caseDuplicates returns the property names to drop: every name that has a
// case-insensitive twin and is not the all-lowercase spelling.
func caseDuplicates(props map[string]any) []string {
	byLower := make(map[string][]string, len(props))
	for name := range props {
		key := bytes.ToLower([]byte(name))
		byLower[string(key)] = append(byLower[string(key)], name)
	}
	var drop []string
	for lower, names := range byLower {
		if len(names) < 2 {
			continue
		}
		for _, name := range names {
			if name != lower {
				drop = append(drop, name)
			}
		}
	}
	sort.Strings(drop)
	return drop
}
