package project

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/janghotan/depscope/internal/model"
)

// TestPnpmLockParsing 验证 pnpm-lock.yaml 格式解析的准确性。
func TestPnpmLockParsing(t *testing.T) {
	sample := `---
lockfileVersion: '9.0'

importers:
  .:
    dependencies:
      left-pad:
        specifier: 1.3.0
        version: 1.3.0

packages:
  left-pad@1.3.0:
    resolution: {integrity: sha512-xxx==}

  '@types/node@20.14.0':
    resolution: {integrity: sha512-yyy==}

snapshots:
  left-pad@1.3.0:
    dependencies:
      '@types/node': 20.14.0

  '@types/node@20.14.0':
    dev: true
`

	entries, incomplete, err := parsePnpmLockfileData([]byte(sample))
	if err != nil {
		t.Fatalf("failed to parse sample pnpm-lock: %v", err)
	}
	if incomplete {
		t.Fatalf("expected complete parsing")
	}

	leftPad, ok := entries["left-pad@1.3.0"]
	if !ok {
		t.Fatalf("missing left-pad@1.3.0 in parsed entries")
	}
	if leftPad.version != "1.3.0" {
		t.Fatalf("left-pad version = %q, want 1.3.0", leftPad.version)
	}
	if leftPad.dependencies["@types/node"] != "20.14.0" {
		t.Fatalf("missing subdependency @types/node: %v", leftPad.dependencies)
	}

	typesNode, ok := entries["@types/node@20.14.0"]
	if !ok {
		t.Fatalf("missing @types/node@20.14.0 in parsed entries")
	}
	if !typesNode.dev {
		t.Fatalf("expected dev=true for @types/node")
	}
}

// TestPnpmProjectScan 验证 pnpm-lock.yaml 项目的候选检测与解析。
func TestPnpmProjectScan(t *testing.T) {
	tempDir := t.TempDir()

	pkgJSON := `{
  "name": "pnpm-demo",
  "version": "1.0.0",
  "dependencies": {
    "left-pad": "^1.3.0"
  }
}`
	pnpmLock := `---
lockfileVersion: '9.0'

snapshots:
  left-pad@1.3.0:
    dependencies:
      undici-types: 5.26.5

  undici-types@5.26.5: {}
`

	if err := os.WriteFile(filepath.Join(tempDir, "package.json"), []byte(pkgJSON), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tempDir, "pnpm-lock.yaml"), []byte(pnpmLock), 0644); err != nil {
		t.Fatal(err)
	}

	detection, err := Detect(tempDir, DefaultLimits())
	if err != nil {
		t.Fatalf("Detect failed: %v", err)
	}
	if len(detection.Candidates) != 1 {
		t.Fatalf("candidates len = %d, want 1", len(detection.Candidates))
	}
	c := detection.Candidates[0]
	if c.Format != FormatNPMPnpmLock {
		t.Fatalf("candidate format = %q, want %q", c.Format, FormatNPMPnpmLock)
	}

	budget := &fileBudget{limits: DefaultLimits()}
	parsed, err := parsePnpmProject(context.Background(), tempDir, c, budget, time.Now)
	if err != nil {
		t.Fatalf("parsePnpmProject failed: %v", err)
	}
	if parsed.fatal {
		t.Fatalf("parsePnpmProject fatal = true")
	}
	if len(parsed.nodes) != 2 {
		t.Fatalf("parsed nodes = %d, want 2 (left-pad direct, undici-types transitive)", len(parsed.nodes))
	}

	var foundDirect, foundTransitive bool
	for _, n := range parsed.nodes {
		if n.Package.Name == "left-pad" && n.Relationship == model.DependencyDirect {
			foundDirect = true
		}
		if n.Package.Name == "undici-types" && n.Relationship == model.DependencyTransitive {
			foundTransitive = true
		}
	}
	if !foundDirect || !foundTransitive {
		t.Fatalf("missing expected direct/transitive relationships: direct=%v, transitive=%v", foundDirect, foundTransitive)
	}
}
