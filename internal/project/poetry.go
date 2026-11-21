package project

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/janghotan/depscope/internal/model"
)

const (
	lockfilePoetryLock    = "poetry.lock"
	manifestPyprojectToml = "pyproject.toml"
	checkPoetryLock       = "poetry:lockfile"
	checkPoetryManifest   = "poetry:manifest"
	projectPoetrySourceID = "project:poetry"
)

// poetryPackage 存储 poetry.lock 中解析的包条目。
type poetryPackage struct {
	name         string
	version      string
	optional     bool
	dependencies map[string]string
}

// poetryDeclaredDep 存储 pyproject.toml 声明的直接依赖。
type poetryDeclaredDep struct {
	name       string
	constraint string
	dev        bool
	optional   bool
}

// parsePoetryProject 解析基于 poetry.lock（以及可选 pyproject.toml）的项目依赖图。
func parsePoetryProject(ctx context.Context, root string, candidate Candidate, budget *fileBudget, clock func() time.Time) (parsedProject, error) {
	result := parsedProject{
		candidate:   candidate,
		nodes:       []Node{},
		checks:      []model.CheckResult{},
		limitations: []model.Limitation{},
	}
	addLimit := limitationCollector(&result.limitations)
	identifier := candidate.LockfilePath
	if candidate.ManifestPath != "" {
		identifier = candidate.ManifestPath + " + " + candidate.LockfilePath
	}
	result.source = model.SourceReference{
		ID:          projectPoetrySourceID,
		Provider:    model.SourceProviderPyPI,
		Identifier:  identifier,
		RetrievedAt: clock().UTC(),
	}

	if err := ctx.Err(); err != nil {
		return result, err
	}

	// 1. 读取 poetry.lock 文件
	lockData, err := readProjectFile(root, candidate.LockfilePath, budget, addLimit)
	if err != nil {
		result.fatal = true
		addLimit("LOCKFILE_UNREADABLE", fmt.Sprintf("poetry.lock could not be read: %v", err))
		result.checks = append(result.checks, model.CheckResult{
			ID:        checkPoetryLock,
			Name:      "Poetry lockfile",
			Status:    model.CheckFailed,
			Message:   fmt.Sprintf("poetry.lock could not be read: %v", err),
			SourceIDs: []string{projectPoetrySourceID},
		})
		result.checks = append(result.checks, unavailableResolutionCheck(model.EcosystemPyPI, "Dependency resolution could not run because poetry.lock was unreadable.", projectPoetrySourceID))
		return result, nil
	}

	pkgs, parseErr := parsePoetryLockfile(lockData)
	if parseErr != nil {
		result.fatal = true
		addLimit("MALFORMED_LOCKFILE", fmt.Sprintf("poetry.lock could not be parsed: %v", parseErr))
		result.checks = append(result.checks, model.CheckResult{
			ID:        checkPoetryLock,
			Name:      "Poetry lockfile",
			Status:    model.CheckFailed,
			Message:   fmt.Sprintf("poetry.lock could not be parsed: %v", parseErr),
			SourceIDs: []string{projectPoetrySourceID},
		})
		result.checks = append(result.checks, unavailableResolutionCheck(model.EcosystemPyPI, "Dependency resolution could not run because poetry.lock was malformed.", projectPoetrySourceID))
		return result, nil
	}

	result.checks = append(result.checks, model.CheckResult{
		ID:        checkPoetryLock,
		Name:      "Poetry lockfile",
		Status:    model.CheckSucceeded,
		Message:   fmt.Sprintf("Read poetry.lock with %d locked package(s).", len(pkgs)),
		SourceIDs: []string{projectPoetrySourceID},
	})

	// 2. 如果存在 pyproject.toml，读取声明的直接依赖与分组
	declared := make(map[string]poetryDeclaredDep)
	if candidate.ManifestPath != "" {
		manifestData, err := readProjectFile(root, candidate.ManifestPath, budget, addLimit)
		if err == nil {
			declared = parsePyprojectDependencies(manifestData)
			result.checks = append(result.checks, model.CheckResult{
				ID:        checkPoetryManifest,
				Name:      "pyproject.toml manifest",
				Status:    model.CheckSucceeded,
				Message:   fmt.Sprintf("Read pyproject.toml with %d declared direct dependency(ies).", len(declared)),
				SourceIDs: []string{projectPoetrySourceID},
			})
		}
	}

	// 3. 构建依赖图节点
	nodes, resolved, unresolved := buildPoetryDependencyGraph(pkgs, declared, candidate.LockfilePath)
	result.nodes = nodes

	resolutionStatus := model.CheckSucceeded
	resolutionMessage := fmt.Sprintf("Resolved %d package(s) from poetry.lock.", resolved)
	if unresolved > 0 {
		resolutionStatus = model.CheckUnavailable
		resolutionMessage = fmt.Sprintf("Resolved %d package(s), but %d package(s) could not be resolved.", resolved, unresolved)
	}
	result.checks = append(result.checks, model.CheckResult{
		ID:        checkPythonResolution,
		Name:      "Python dependency resolution",
		Status:    resolutionStatus,
		Message:   resolutionMessage,
		SourceIDs: []string{projectPoetrySourceID},
	})

	return result, nil
}

// parsePoetryLockfile 解析 poetry.lock 包含的 [[package]] 块。
func parsePoetryLockfile(data []byte) (map[string]*poetryPackage, error) {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	buf := make([]byte, 64*1024)
	scanner.Buffer(buf, 8*1024*1024)

	packages := make(map[string]*poetryPackage)
	var current *poetryPackage
	inDependencies := false

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		if line == "[[package]]" {
			if current != nil && current.name != "" {
				packages[strings.ToLower(current.name)] = current
			}
			current = &poetryPackage{
				dependencies: make(map[string]string),
			}
			inDependencies = false
			continue
		}

		if strings.HasPrefix(line, "[") {
			if line == "[package.dependencies]" {
				inDependencies = true
			} else {
				inDependencies = false
			}
			continue
		}

		if current == nil {
			continue
		}

		if inDependencies {
			parts := strings.SplitN(line, "=", 2)
			if len(parts) == 2 {
				depName := strings.Trim(strings.TrimSpace(parts[0]), `'"`)
				depConstraint := strings.Trim(strings.TrimSpace(parts[1]), `'"`)
				if depName != "" {
					current.dependencies[strings.ToLower(depName)] = depConstraint
				}
			}
			continue
		}

		parts := strings.SplitN(line, "=", 2)
		if len(parts) == 2 {
			key := strings.TrimSpace(parts[0])
			val := strings.Trim(strings.TrimSpace(parts[1]), `'"`)
			switch key {
			case "name":
				identity, err := model.NewPackageIdentity(model.EcosystemPyPI, val)
				if err == nil {
					current.name = identity.Name
				} else {
					current.name = val
				}
			case "version":
				current.version = val
			case "optional":
				current.optional = (val == "true")
			}
		}
	}

	if current != nil && current.name != "" {
		packages[strings.ToLower(current.name)] = current
	}

	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return packages, nil
}

// parsePyprojectDependencies 从 pyproject.toml 提取声明的依赖。
func parsePyprojectDependencies(data []byte) map[string]poetryDeclaredDep {
	declared := make(map[string]poetryDeclaredDep)
	scanner := bufio.NewScanner(bytes.NewReader(data))
	currentSection := ""

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			currentSection = strings.Trim(line, "[] ")
			continue
		}

		isMainDep := currentSection == "tool.poetry.dependencies"
		isDevDep := strings.HasPrefix(currentSection, "tool.poetry.group.dev") || currentSection == "tool.poetry.dev-dependencies"

		if isMainDep || isDevDep {
			parts := strings.SplitN(line, "=", 2)
			if len(parts) == 2 {
				depName := strings.Trim(strings.TrimSpace(parts[0]), `'"`)
				if strings.ToLower(depName) == "python" {
					continue
				}
				constraint := strings.Trim(strings.TrimSpace(parts[1]), `'"`)
				canonical := depName
				identity, err := model.NewPackageIdentity(model.EcosystemPyPI, depName)
				if err == nil {
					canonical = identity.Name
				}
				declared[strings.ToLower(canonical)] = poetryDeclaredDep{
					name:       canonical,
					constraint: constraint,
					dev:        isDevDep,
				}
			}
		}
	}

	return declared
}

// buildPoetryDependencyGraph 将 poetryPackage 列表转换为 Node 图。
func buildPoetryDependencyGraph(
	pkgs map[string]*poetryPackage,
	declared map[string]poetryDeclaredDep,
	lockfilePath string,
) ([]Node, int, int) {
	nodeMap := make(map[string]*Node)
	resolvedCount := 0

	hasDeclared := len(declared) > 0
	incoming := make(map[string]int)
	for _, p := range pkgs {
		for depName := range p.dependencies {
			incoming[strings.ToLower(depName)]++
		}
	}

	for _, p := range pkgs {
		canonicalName := p.name
		lowerName := strings.ToLower(canonicalName)
		nodeID := fmt.Sprintf("pypi:%s@%s", canonicalName, p.version)

		isDirect := false
		scope := DependencyScopeProduction
		constraint := ""

		if hasDeclared {
			if dec, ok := declared[lowerName]; ok {
				isDirect = true
				constraint = dec.constraint
				if dec.dev {
					scope = DependencyScopeDevelopment
				}
			}
		} else {
			if incoming[lowerName] == 0 {
				isDirect = true
			}
		}

		rel := model.DependencyTransitive
		if isDirect {
			rel = model.DependencyDirect
		}
		if p.optional {
			scope = DependencyScopeOptional
		}

		depIDs := make([]string, 0, len(p.dependencies))
		for childName := range p.dependencies {
			if childPkg, ok := pkgs[strings.ToLower(childName)]; ok {
				childID := fmt.Sprintf("pypi:%s@%s", childPkg.name, childPkg.version)
				depIDs = append(depIDs, childID)
			}
		}
		sort.Strings(depIDs)

		resolvedCount++
		nodeMap[nodeID] = &Node{
			DependencyNode: model.DependencyNode{
				ID:             nodeID,
				Package:        model.PackageIdentity{Ecosystem: model.EcosystemPyPI, Name: canonicalName},
				Version:        stringPointer(p.version),
				Relationship:   rel,
				DependencyPath: []string{rootNodeID, nodeID},
				Dependencies:   depIDs,
			},
			Constraint: constraint,
			Scopes:     []DependencyScope{scope},
			ScopeEvidence: []DependencyEvidence{
				{Path: lockfilePath, Field: "[[package]]"},
			},
		}
	}

	nodes := make([]Node, 0, len(nodeMap))
	for _, n := range nodeMap {
		nodes = append(nodes, *n)
	}
	sort.Slice(nodes, func(i, j int) bool {
		return nodes[i].ID < nodes[j].ID
	})

	return nodes, resolvedCount, 0
}
