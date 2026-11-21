package project

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/janghotan/depscope/internal/model"
)

// TestPoetryLockParsing 验证 poetry.lock 文件解析的准确性。
func TestPoetryLockParsing(t *testing.T) {
	sample := `
[[package]]
name = "requests"
version = "2.31.0"
description = "Python HTTP for Humans."
optional = false
python-versions = ">=3.8"

[package.dependencies]
certifi = ">=2017.4.17"
urllib3 = ">=1.21.1,<3"

[[package]]
name = "urllib3"
version = "2.0.7"
optional = false

[[package]]
name = "certifi"
version = "2023.7.22"
optional = false
`

	pkgs, err := parsePoetryLockfile([]byte(sample))
	if err != nil {
		t.Fatalf("failed to parse sample poetry.lock: %v", err)
	}
	if len(pkgs) != 3 {
		t.Fatalf("packages count = %d, want 3", len(pkgs))
	}

	req, ok := pkgs["requests"]
	if !ok {
		t.Fatalf("missing requests in parsed packages")
	}
	if req.version != "2.31.0" {
		t.Fatalf("requests version = %q, want 2.31.0", req.version)
	}
	if len(req.dependencies) != 2 {
		t.Fatalf("requests dependencies count = %d, want 2", len(req.dependencies))
	}
}

// TestPoetryProjectScan 验证 poetry.lock 项目的检测与依赖图构建。
func TestPoetryProjectScan(t *testing.T) {
	tempDir := t.TempDir()

	pyproject := `
[tool.poetry]
name = "poetry-demo"
version = "0.1.0"

[tool.poetry.dependencies]
python = "^3.10"
requests = "^2.31.0"

[tool.poetry.group.dev.dependencies]
pytest = "^7.4.0"
`

	poetryLock := `
[[package]]
name = "requests"
version = "2.31.0"
optional = false

[package.dependencies]
urllib3 = ">=1.21.1,<3"

[[package]]
name = "urllib3"
version = "2.0.7"
optional = false

[[package]]
name = "pytest"
version = "7.4.0"
optional = false
`

	if err := os.WriteFile(filepath.Join(tempDir, "pyproject.toml"), []byte(pyproject), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tempDir, "poetry.lock"), []byte(poetryLock), 0644); err != nil {
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
	if c.Format != FormatPythonPoetryLock {
		t.Fatalf("candidate format = %q, want %q", c.Format, FormatPythonPoetryLock)
	}

	budget := &fileBudget{limits: DefaultLimits()}
	parsed, err := parsePoetryProject(context.Background(), tempDir, c, budget, time.Now)
	if err != nil {
		t.Fatalf("parsePoetryProject failed: %v", err)
	}
	if parsed.fatal {
		t.Fatalf("parsePoetryProject fatal = true")
	}
	if len(parsed.nodes) != 3 {
		t.Fatalf("parsed nodes count = %d, want 3", len(parsed.nodes))
	}

	var foundRequests, foundUrllib3, foundPytest bool
	for _, n := range parsed.nodes {
		if n.Package.Name == "requests" && n.Relationship == model.DependencyDirect {
			foundRequests = true
		}
		if n.Package.Name == "urllib3" && n.Relationship == model.DependencyTransitive {
			foundUrllib3 = true
		}
		if n.Package.Name == "pytest" && n.Relationship == model.DependencyDirect && n.Scopes[0] == DependencyScopeDevelopment {
			foundPytest = true
		}
	}

	if !foundRequests || !foundUrllib3 || !foundPytest {
		t.Fatalf("failed expectations: requests=%v, urllib3=%v, pytest=%v", foundRequests, foundUrllib3, foundPytest)
	}
}
