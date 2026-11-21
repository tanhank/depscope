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
	lockfilePnpmLock    = "pnpm-lock.yaml"
	checkPnpmLock       = "pnpm:lockfile"
	projectPnpmSourceID = "project:pnpm"
)

// pnpmParsedEntry 存储 pnpm-lock.yaml 中单项的已解析元数据。
type pnpmParsedEntry struct {
	name         string
	version      string
	dev          bool
	optional     bool
	dependencies map[string]string
}

// parsePnpmProject 解析基于 pnpm-lock.yaml 的项目依赖图。
func parsePnpmProject(ctx context.Context, root string, candidate Candidate, budget *fileBudget, clock func() time.Time) (parsedProject, error) {
	result := parsedProject{
		candidate:   candidate,
		nodes:       []Node{},
		checks:      []model.CheckResult{},
		limitations: []model.Limitation{},
	}
	addLimit := limitationCollector(&result.limitations)
	identifier := candidate.ManifestPath
	if candidate.LockfilePath != "" {
		identifier += " + " + candidate.LockfilePath
	}
	result.source = model.SourceReference{
		ID:          projectPnpmSourceID,
		Provider:    model.SourceProviderNPM,
		Identifier:  identifier,
		RetrievedAt: clock().UTC(),
	}

	if err := ctx.Err(); err != nil {
		return result, err
	}

	// 1. 读取 package.json 清单
	manifestData, err := readProjectFile(root, candidate.ManifestPath, budget, addLimit)
	if err != nil {
		result.fatal = true
		addLimit("MANIFEST_UNREADABLE", fmt.Sprintf("package.json could not be read: %v", err))
		result.checks = append(result.checks, model.CheckResult{
			ID:        checkNPMManifest,
			Name:      "npm manifest",
			Status:    model.CheckFailed,
			Message:   fmt.Sprintf("package.json could not be read: %v", err),
			SourceIDs: []string{projectPnpmSourceID},
		})
		result.checks = append(result.checks, unavailableResolutionCheck(model.EcosystemNPM, "Dependency resolution could not run because package.json was unreadable.", projectPnpmSourceID))
		return result, nil
	}
	var manifest npmManifest
	if err := decodeJSONDocument(manifestData, &manifest); err != nil {
		result.fatal = true
		addLimit("MALFORMED_MANIFEST", fmt.Sprintf("package.json is not valid JSON: %v", err))
		result.checks = append(result.checks, model.CheckResult{
			ID:        checkNPMManifest,
			Name:      "npm manifest",
			Status:    model.CheckFailed,
			Message:   fmt.Sprintf("package.json is not valid JSON: %v", err),
			SourceIDs: []string{projectPnpmSourceID},
		})
		result.checks = append(result.checks, unavailableResolutionCheck(model.EcosystemNPM, "Dependency resolution could not run because package.json was malformed.", projectPnpmSourceID))
		return result, nil
	}

	declared, manifestIncomplete := npmDeclaredDependencies(manifest, addLimit)
	manifestName := strings.TrimSpace(manifest.Name)
	if manifestName == "" {
		manifestName = "unnamed npm project"
	}
	manifestMessage := fmt.Sprintf("Read package.json for %s with %d declared direct dependency(ies).", manifestName, len(declared))
	manifestStatus := model.CheckSucceeded
	if manifestIncomplete {
		manifestStatus = model.CheckUnavailable
		manifestMessage += " Some declared dependency metadata could not be interpreted."
	}
	result.checks = append(result.checks, model.CheckResult{
		ID:        checkNPMManifest,
		Name:      "npm manifest",
		Status:    manifestStatus,
		Message:   manifestMessage,
		SourceIDs: []string{projectPnpmSourceID},
	})

	if candidate.LockfilePath == "" {
		result.checks = append(result.checks, model.CheckResult{
			ID:        checkPnpmLock,
			Name:      "pnpm lockfile",
			Status:    model.CheckUnavailable,
			Message:   "pnpm-lock.yaml is missing; dependency versions cannot be resolved.",
			SourceIDs: []string{projectPnpmSourceID},
		})
		npmUnresolvedDirectNodes(&result, declared, budget.limits)
		result.checks = append(result.checks, unavailableResolutionCheck(
			model.EcosystemNPM,
			fmt.Sprintf("Resolved 0 of %d direct dependency(ies); pnpm-lock.yaml is required to determine installed versions.", len(declared)),
			projectPnpmSourceID,
		))
		return result, nil
	}

	// 2. 读取并解析 pnpm-lock.yaml
	lockData, err := readProjectFile(root, candidate.LockfilePath, budget, addLimit)
	if err != nil {
		result.fatal = true
		addLimit("LOCKFILE_UNREADABLE", fmt.Sprintf("pnpm-lock.yaml could not be read: %v", err))
		result.checks = append(result.checks, model.CheckResult{
			ID:        checkPnpmLock,
			Name:      "pnpm lockfile",
			Status:    model.CheckFailed,
			Message:   fmt.Sprintf("pnpm-lock.yaml could not be read: %v", err),
			SourceIDs: []string{projectPnpmSourceID},
		})
		result.checks = append(result.checks, unavailableResolutionCheck(model.EcosystemNPM, "Dependency resolution could not run because pnpm-lock.yaml was unreadable.", projectPnpmSourceID))
		return result, nil
	}

	entries, lockfileIncomplete, parseErr := parsePnpmLockfileData(lockData)
	if parseErr != nil {
		result.fatal = true
		addLimit("MALFORMED_LOCKFILE", fmt.Sprintf("pnpm-lock.yaml could not be parsed: %v", parseErr))
		result.checks = append(result.checks, model.CheckResult{
			ID:        checkPnpmLock,
			Name:      "pnpm lockfile",
			Status:    model.CheckFailed,
			Message:   fmt.Sprintf("pnpm-lock.yaml could not be parsed: %v", parseErr),
			SourceIDs: []string{projectPnpmSourceID},
		})
		result.checks = append(result.checks, unavailableResolutionCheck(model.EcosystemNPM, "Dependency resolution could not run because pnpm-lock.yaml was malformed.", projectPnpmSourceID))
		return result, nil
	}

	lockStatus := model.CheckSucceeded
	lockMessage := fmt.Sprintf("Read pnpm-lock.yaml with %d resolved package entry(ies).", len(entries))
	if lockfileIncomplete {
		lockStatus = model.CheckUnavailable
		lockMessage += " Some lockfile entries could not be fully parsed."
	}
	result.checks = append(result.checks, model.CheckResult{
		ID:        checkPnpmLock,
		Name:      "pnpm lockfile",
		Status:    lockStatus,
		Message:   lockMessage,
		SourceIDs: []string{projectPnpmSourceID},
	})

	// 3. 构建依赖图
	nodes, resolved, unresolved := buildPnpmDependencyGraph(declared, entries, candidate.LockfilePath, addLimit, budget.limits)
	result.nodes = nodes

	resolutionStatus := model.CheckSucceeded
	resolutionMessage := fmt.Sprintf("Resolved %d package(s) from pnpm-lock.yaml.", resolved)
	if unresolved > 0 {
		resolutionStatus = model.CheckUnavailable
		resolutionMessage = fmt.Sprintf("Resolved %d package(s), but %d package(s) could not be resolved.", resolved, unresolved)
	}
	result.checks = append(result.checks, model.CheckResult{
		ID:        checkNPMResolution,
		Name:      "npm dependency resolution",
		Status:    resolutionStatus,
		Message:   resolutionMessage,
		SourceIDs: []string{projectPnpmSourceID},
	})

	return result, nil
}

// parsePnpmKey 解析诸如 '/@scope/pkg@1.2.3' 或 'pkg@1.2.3(peer@...)' 的键名。
func parsePnpmKey(key string) (name, version string, ok bool) {
	s := strings.TrimSpace(key)
	s = strings.Trim(s, `'"`)
	s = strings.TrimSuffix(s, ":")
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "/")

	if idx := strings.Index(s, "("); idx != -1 {
		s = s[:idx]
	}

	lastAt := strings.LastIndex(s, "@")
	if lastAt <= 0 || lastAt >= len(s)-1 {
		return "", "", false
	}
	name = s[:lastAt]
	version = s[lastAt+1:]
	if name == "" || version == "" {
		return "", "", false
	}
	return name, version, true
}

// parsePnpmLockfileData 解析 pnpm-lock.yaml 字节数据。
func parsePnpmLockfileData(data []byte) (map[string]*pnpmParsedEntry, bool, error) {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	buf := make([]byte, 64*1024)
	scanner.Buffer(buf, 8*1024*1024)

	entries := make(map[string]*pnpmParsedEntry)
	var currentEntry *pnpmParsedEntry
	inSection := ""
	inSubSection := ""
	incomplete := false

	for scanner.Scan() {
		rawLine := scanner.Text()
		trimmed := strings.TrimSpace(rawLine)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") || trimmed == "---" {
			continue
		}

		indent := len(rawLine) - len(strings.TrimLeft(rawLine, " "))

		if indent == 0 {
			currentEntry = nil
			inSubSection = ""
			if strings.HasPrefix(trimmed, "packages:") {
				inSection = "packages"
			} else if strings.HasPrefix(trimmed, "snapshots:") {
				inSection = "snapshots"
			} else if strings.HasPrefix(trimmed, "importers:") {
				inSection = "importers"
			} else {
				inSection = ""
			}
			continue
		}

		if inSection == "packages" || inSection == "snapshots" {
			if (indent == 2 && strings.HasSuffix(trimmed, ":")) || (indent == 2 && strings.Contains(trimmed, "@")) {
				colonIdx := strings.Index(trimmed, ":")
				keyPart := trimmed
				if colonIdx != -1 {
					keyPart = trimmed[:colonIdx]
				}
				name, ver, ok := parsePnpmKey(keyPart)
				if ok {
					entryKey := name + "@" + ver
					if existing, exists := entries[entryKey]; exists {
						currentEntry = existing
					} else {
						currentEntry = &pnpmParsedEntry{
							name:         name,
							version:      ver,
							dependencies: make(map[string]string),
						}
						entries[entryKey] = currentEntry
					}
					inSubSection = ""
					continue
				}
			}

			if currentEntry != nil {
				if indent == 4 {
					if strings.HasPrefix(trimmed, "dependencies:") {
						inSubSection = "dependencies"
						continue
					}
					if strings.HasPrefix(trimmed, "optionalDependencies:") {
						inSubSection = "optionalDependencies"
						continue
					}
					if strings.HasPrefix(trimmed, "dev:") {
						val := strings.TrimSpace(strings.TrimPrefix(trimmed, "dev:"))
						if val == "true" {
							currentEntry.dev = true
						}
						inSubSection = ""
						continue
					}
					if strings.HasPrefix(trimmed, "optional:") {
						val := strings.TrimSpace(strings.TrimPrefix(trimmed, "optional:"))
						if val == "true" {
							currentEntry.optional = true
						}
						inSubSection = ""
						continue
					}
					inSubSection = ""
				}

				if (inSubSection == "dependencies" || inSubSection == "optionalDependencies") && indent >= 6 {
					parts := strings.SplitN(trimmed, ":", 2)
					if len(parts) == 2 {
						depName := strings.Trim(strings.TrimSpace(parts[0]), `'"`)
						depVer := strings.Trim(strings.TrimSpace(parts[1]), `'"`)
						if idx := strings.Index(depVer, "("); idx != -1 {
							depVer = depVer[:idx]
						}
						if depName != "" && depVer != "" {
							currentEntry.dependencies[depName] = depVer
						}
					}
				}
			}
		}
	}

	if err := scanner.Err(); err != nil {
		return entries, true, err
	}
	return entries, incomplete, nil
}

// buildPnpmDependencyGraph 将清单声明与已解析的 pnpm 条目合并为 Node 列表。
func buildPnpmDependencyGraph(
	declared map[string]npmDeclaredDependency,
	entries map[string]*pnpmParsedEntry,
	lockfilePath string,
	addLimit func(code, message string),
	limits Limits,
) ([]Node, int, int) {
	nodeMap := make(map[string]*Node)
	resolvedCount := 0
	unresolvedCount := 0

	byName := make(map[string][]*pnpmParsedEntry)
	for _, entry := range entries {
		byName[entry.name] = append(byName[entry.name], entry)
	}
	for name := range byName {
		sort.Slice(byName[name], func(i, j int) bool {
			return byName[name][i].version < byName[name][j].version
		})
	}

	for _, dep := range declared {
		matching := byName[dep.name]
		if len(matching) == 0 {
			unresolvedCount++
			addLimit("DIRECT_DEPENDENCY_UNRESOLVED", fmt.Sprintf("Direct dependency %q could not be matched in %s.", dep.name, lockfilePath))
			nodeID := "npm:node_modules/" + dep.name
			if _, exists := nodeMap[nodeID]; !exists {
				nodeMap[nodeID] = &Node{
					DependencyNode: model.DependencyNode{
						ID:             nodeID,
						Package:        model.PackageIdentity{Ecosystem: model.EcosystemNPM, Name: dep.name},
						Relationship:   model.DependencyDirect,
						DependencyPath: []string{rootNodeID, nodeID},
						Dependencies:   []string{},
					},
					Constraint:    dep.constraint,
					Scopes:        dep.scopes,
					ScopeEvidence: npmManifestEvidence(dep.fields),
				}
			}
			continue
		}

		chosen := matching[0]
		resolvedCount++
		nodeID := "npm:node_modules/" + chosen.name

		scopes := dep.scopes
		if chosen.optional {
			scopes = append(scopes, DependencyScopeOptional)
		}
		if chosen.dev {
			scopes = append(scopes, DependencyScopeDevelopment)
		}

		evidence := append(npmManifestEvidence(dep.fields), DependencyEvidence{Path: lockfilePath})

		nodeMap[nodeID] = &Node{
			DependencyNode: model.DependencyNode{
				ID:             nodeID,
				Package:        model.PackageIdentity{Ecosystem: model.EcosystemNPM, Name: chosen.name},
				Version:        stringPointer(chosen.version),
				Relationship:   model.DependencyDirect,
				DependencyPath: []string{rootNodeID, nodeID},
				Dependencies:   []string{},
			},
			Constraint:    dep.constraint,
			Scopes:        scopes,
			ScopeEvidence: normalizeEvidence(evidence),
		}
	}

	queue := make([]string, 0, len(nodeMap))
	for id := range nodeMap {
		queue = append(queue, id)
	}

	visited := make(map[string]bool)
	for len(queue) > 0 {
		currID := queue[0]
		queue = queue[1:]

		if visited[currID] {
			continue
		}
		visited[currID] = true

		currNode := nodeMap[currID]
		if currNode == nil || currNode.Version == nil {
			continue
		}

		key := currNode.Package.Name + "@" + *currNode.Version
		entry := entries[key]
		if entry == nil {
			continue
		}

		for childName, childVer := range entry.dependencies {
			childKey := childName + "@" + childVer
			childEntry := entries[childKey]
			childID := "npm:node_modules/" + childName

			if _, exists := nodeMap[childID]; !exists {
				scope := DependencyScopeProduction
				if childEntry != nil && childEntry.dev {
					scope = DependencyScopeDevelopment
				} else if childEntry != nil && childEntry.optional {
					scope = DependencyScopeOptional
				}
				resolvedCount++
				nodeMap[childID] = &Node{
					DependencyNode: model.DependencyNode{
						ID:             childID,
						Package:        model.PackageIdentity{Ecosystem: model.EcosystemNPM, Name: childName},
						Version:        stringPointer(childVer),
						Relationship:   model.DependencyTransitive,
						DependencyPath: append(append([]string{}, currNode.DependencyPath...), childID),
						Dependencies:   []string{},
					},
					Scopes: []DependencyScope{scope},
					ScopeEvidence: []DependencyEvidence{
						{Path: lockfilePath, Field: "packages." + key + ".dependencies"},
					},
				}
				queue = append(queue, childID)
			}
			currNode.Dependencies = appendUnique(currNode.Dependencies, childID)
		}
	}

	nodes := make([]Node, 0, len(nodeMap))
	for _, n := range nodeMap {
		sort.Strings(n.Dependencies)
		nodes = append(nodes, *n)
	}
	sort.Slice(nodes, func(i, j int) bool {
		return nodes[i].ID < nodes[j].ID
	})

	return nodes, resolvedCount, unresolvedCount
}

func appendUnique(slice []string, val string) []string {
	for _, item := range slice {
		if item == val {
			return slice
		}
	}
	return append(slice, val)
}
