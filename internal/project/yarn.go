package project

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/janghotan/depscope/internal/model"
)

const (
	lockfileYarnLock    = "yarn.lock"
	checkYarnLock       = "yarn:lockfile"
	projectYarnSourceID = "project:yarn"
)

// yarnParsedEntry 存储 yarn.lock 中单项的已解析元数据。
type yarnParsedEntry struct {
	name         string
	version      string
	dependencies map[string]string
}

// parseYarnProject 解析基于 yarn.lock v1 的项目依赖图。
func parseYarnProject(ctx context.Context, root string, candidate Candidate, budget *fileBudget, clock func() time.Time) (parsedProject, error) {
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
		ID:          projectYarnSourceID,
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
			SourceIDs: []string{projectYarnSourceID},
		})
		result.checks = append(result.checks, unavailableResolutionCheck(model.EcosystemNPM, "Dependency resolution could not run because package.json was unreadable.", projectYarnSourceID))
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
			SourceIDs: []string{projectYarnSourceID},
		})
		result.checks = append(result.checks, unavailableResolutionCheck(model.EcosystemNPM, "Dependency resolution could not run because package.json was malformed.", projectYarnSourceID))
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
		SourceIDs: []string{projectYarnSourceID},
	})

	if candidate.LockfilePath == "" {
		result.checks = append(result.checks, model.CheckResult{
			ID:        checkYarnLock,
			Name:      "yarn lockfile",
			Status:    model.CheckUnavailable,
			Message:   "yarn.lock is missing; resolved versions are unavailable and latest versions are never substituted.",
			SourceIDs: []string{projectYarnSourceID},
		})
		npmUnresolvedDirectNodes(&result, declared, budget.limits)
		result.checks = append(result.checks, unavailableResolutionCheck(
			model.EcosystemNPM,
			fmt.Sprintf("Resolved 0 of %d direct dependency(ies); yarn.lock is required to determine installed versions.", len(declared)),
			projectYarnSourceID,
		))
		return result, nil
	}

	if err := ctx.Err(); err != nil {
		return result, err
	}

	// 2. 读取 yarn.lock
	lockData, err := readProjectFile(root, candidate.LockfilePath, budget, addLimit)
	if err != nil {
		result.fatal = true
		addLimit("LOCKFILE_UNREADABLE", fmt.Sprintf("yarn.lock could not be read: %v", err))
		result.checks = append(result.checks, model.CheckResult{
			ID:        checkYarnLock,
			Name:      "yarn lockfile",
			Status:    model.CheckFailed,
			Message:   fmt.Sprintf("yarn.lock could not be read: %v", err),
			SourceIDs: []string{projectYarnSourceID},
		})
		result.checks = append(result.checks, unavailableResolutionCheck(model.EcosystemNPM, "Dependency resolution could not run because yarn.lock was unreadable.", projectYarnSourceID))
		return result, nil
	}

	entriesByPattern, err := parseYarnLockDocument(lockData)
	if err != nil {
		result.fatal = true
		addLimit("MALFORMED_LOCKFILE", fmt.Sprintf("yarn.lock is malformed: %v", err))
		result.checks = append(result.checks, model.CheckResult{
			ID:        checkYarnLock,
			Name:      "yarn lockfile",
			Status:    model.CheckFailed,
			Message:   fmt.Sprintf("yarn.lock is malformed: %v", err),
			SourceIDs: []string{projectYarnSourceID},
		})
		result.checks = append(result.checks, unavailableResolutionCheck(model.EcosystemNPM, "Dependency resolution could not run because yarn.lock was malformed.", projectYarnSourceID))
		return result, nil
	}

	result.checks = append(result.checks, model.CheckResult{
		ID:        checkYarnLock,
		Name:      "yarn lockfile",
		Status:    model.CheckSucceeded,
		Message:   fmt.Sprintf("Read yarn.lock v1 with %d package resolution pattern(s).", len(entriesByPattern)),
		SourceIDs: []string{projectYarnSourceID},
	})

	// 3. 构建依赖图
	builder := yarnGraphBuilder{
		entries:      entriesByPattern,
		lockfilePath: candidate.LockfilePath,
		limits:       budget.limits,
		nodes:        []Node{},
		nodeIDs:      make(map[string]struct{}),
		visited:      make(map[string]struct{}),
		queue:        []yarnQueueItem{},
		addLimit:     addLimit,
	}
	builder.build(declared)
	result.nodes = builder.nodes

	directCount := len(declared)
	resolvedCount := builder.resolved
	unresolvedCount := builder.unresolved
	resolutionIncomplete := manifestIncomplete || builder.incomplete
	resolutionStatus := model.CheckSucceeded
	resolutionMessage := fmt.Sprintf("Resolved %d package instance(s) from yarn.lock for %d direct dependency(ies); %d dependency edge(s) remain unresolved.", resolvedCount, directCount, unresolvedCount)
	if resolutionIncomplete {
		resolutionStatus = model.CheckUnavailable
		resolutionMessage = fmt.Sprintf("Dependency resolution is incomplete: %d package instance(s) resolved for %d direct dependency(ies), %d unresolved. Unresolved packages are not scanned and latest versions are never substituted.", resolvedCount, directCount, unresolvedCount)
	}
	result.checks = append(result.checks, model.CheckResult{
		ID:        checkNPMResolution,
		Name:      "npm dependency resolution",
		Status:    resolutionStatus,
		Message:   resolutionMessage,
		SourceIDs: []string{projectYarnSourceID},
	})

	return result, nil
}

// parseYarnLockDocument 解析 yarn.lock v1 文件。
func parseYarnLockDocument(data []byte) (map[string]*yarnParsedEntry, error) {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	entries := make(map[string]*yarnParsedEntry)

	var currentEntry *yarnParsedEntry
	var currentPatterns []string
	inDependencies := false

	for scanner.Scan() {
		rawLine := scanner.Text()
		trimmed := strings.TrimSpace(rawLine)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}

		// 非缩进行表示新的包模式头
		if !strings.HasPrefix(rawLine, " ") && !strings.HasPrefix(rawLine, "\t") {
			inDependencies = false
			header := strings.TrimSuffix(trimmed, ":")
			parts := strings.Split(header, ",")
			currentPatterns = make([]string, 0, len(parts))
			for _, part := range parts {
				p := strings.Trim(strings.TrimSpace(part), `"'`)
				if p != "" {
					currentPatterns = append(currentPatterns, p)
				}
			}
			currentEntry = &yarnParsedEntry{
				dependencies: make(map[string]string),
			}
			for _, pat := range currentPatterns {
				entries[pat] = currentEntry
			}
			continue
		}

		if currentEntry == nil {
			continue
		}

		// 处理 version 字段
		if strings.HasPrefix(trimmed, "version ") {
			inDependencies = false
			ver := strings.TrimPrefix(trimmed, "version ")
			currentEntry.version = strings.Trim(strings.TrimSpace(ver), `"'`)
			continue
		}

		// 处理 dependencies 分节
		if trimmed == "dependencies:" || trimmed == "optionalDependencies:" {
			inDependencies = true
			continue
		}

		if inDependencies {
			// 解析依赖条目行: "pkg-name" "constraint"
			fields := strings.Fields(trimmed)
			if len(fields) >= 2 {
				depName := strings.Trim(fields[0], `"'`)
				depConstraint := strings.Trim(fields[1], `"'`)
				currentEntry.dependencies[depName] = depConstraint
			}
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return entries, nil
}

type yarnQueueItem struct {
	name       string
	constraint string
	parentID   string
	path       []string
}

type yarnGraphBuilder struct {
	entries      map[string]*yarnParsedEntry
	lockfilePath string
	limits       Limits
	nodes        []Node
	nodeIDs      map[string]struct{}
	visited      map[string]struct{}
	queue        []yarnQueueItem
	addLimit     func(code, message string)
	incomplete   bool
	stopped      bool
	resolved     int
	unresolved   int
	nodeIndex    map[string]int
}

func (b *yarnGraphBuilder) findEntry(name, constraint string) (*yarnParsedEntry, bool) {
	// 1. 优先尝试完全匹配 pattern: name@constraint
	exactPattern := name + "@" + constraint
	if entry, exists := b.entries[exactPattern]; exists && entry.version != "" {
		return entry, true
	}

	// 2. 尝试前缀匹配 name@
	prefix := name + "@"
	for pat, entry := range b.entries {
		if strings.HasPrefix(pat, prefix) && entry.version != "" {
			return entry, true
		}
	}
	return nil, false
}

func (b *yarnGraphBuilder) build(declared map[string]npmDeclaredDependency) {
	for _, name := range sortedKeysFromDeclared(declared) {
		dep := declared[name]
		pkg, err := model.NewPackageIdentity(model.EcosystemNPM, dep.name)
		if err != nil {
			b.incomplete = true
			continue
		}

		entry, found := b.findEntry(dep.name, dep.constraint)
		if !found {
			b.addUnresolvedDirect(pkg, dep)
			b.incomplete = true
			continue
		}

		nodeID := "npm:node_modules/" + pkg.Name
		evidence := append(npmManifestEvidence(dep.fields), DependencyEvidence{Path: b.lockfilePath})
		node := Node{
			DependencyNode: model.DependencyNode{
				ID:             nodeID,
				Package:        pkg,
				Version:        stringPointer(entry.version),
				Relationship:   model.DependencyDirect,
				DependencyPath: []string{rootNodeID, nodeID},
				Dependencies:   []string{},
			},
			Constraint:    dep.constraint,
			Scopes:        dep.scopes,
			ScopeEvidence: normalizeEvidence(evidence),
		}
		if !b.appendNode(node) {
			return
		}
		b.resolved++
		b.visited[pkg.Name] = struct{}{}
		b.queue = append(b.queue, yarnQueueItem{
			name:       dep.name,
			constraint: dep.constraint,
			parentID:   nodeID,
			path:       node.DependencyPath,
		})
	}

	// 遍历传递依赖
	for index := 0; index < len(b.queue) && !b.stopped; index++ {
		item := b.queue[index]
		entry, found := b.findEntry(item.name, item.constraint)
		if !found {
			continue
		}

		for subName, subConstraint := range entry.dependencies {
			if b.stopped {
				break
			}
			pkg, err := model.NewPackageIdentity(model.EcosystemNPM, subName)
			if err != nil {
				b.incomplete = true
				continue
			}
			depth := len(item.path)
			if depth > b.limits.MaxDepth {
				b.addLimit("DEPENDENCY_DEPTH_EXCEEDED", fmt.Sprintf("Dependencies deeper than %d package levels were not traversed; coverage is incomplete.", b.limits.MaxDepth))
				b.incomplete = true
				b.stopped = true
				break
			}

			subEntry, subFound := b.findEntry(subName, subConstraint)
			if !subFound {
				unresolvedID := "npm:unresolved:" + subName
				b.appendUnresolvedTransitive(item, unresolvedID, pkg, subConstraint)
				b.incomplete = true
				continue
			}

			nodeID := "npm:node_modules/" + subName
			if _, seen := b.visited[subName]; seen {
				b.addEdge(item.parentID, nodeID)
				continue
			}

			node := Node{
				DependencyNode: model.DependencyNode{
					ID:             nodeID,
					Package:        pkg,
					Version:        stringPointer(subEntry.version),
					Relationship:   model.DependencyTransitive,
					DependencyPath: append(append([]string(nil), item.path...), nodeID),
					Dependencies:   []string{},
				},
				Constraint:    subConstraint,
				Scopes:        []DependencyScope{DependencyScopeProduction},
				ScopeEvidence: []DependencyEvidence{{Path: b.lockfilePath}},
			}
			if !b.appendNode(node) {
				break
			}
			b.visited[subName] = struct{}{}
			b.resolved++
			b.addEdge(item.parentID, nodeID)
			b.queue = append(b.queue, yarnQueueItem{
				name:       subName,
				constraint: subConstraint,
				parentID:   nodeID,
				path:       node.DependencyPath,
			})
		}
	}
}

func (b *yarnGraphBuilder) appendNode(node Node) bool {
	if len(b.nodes) >= b.limits.MaxNodes {
		b.addLimit("NODE_LIMIT_EXCEEDED", fmt.Sprintf("The dependency graph is limited to %d nodes; additional packages were not traversed.", b.limits.MaxNodes))
		b.incomplete = true
		b.stopped = true
		return false
	}
	if _, exists := b.nodeIDs[node.ID]; exists {
		return false
	}
	b.nodeIDs[node.ID] = struct{}{}
	if b.nodeIndex == nil {
		b.nodeIndex = make(map[string]int)
	}
	b.nodeIndex[node.ID] = len(b.nodes)
	b.nodes = append(b.nodes, node)
	return true
}

func (b *yarnGraphBuilder) addUnresolvedDirect(pkg model.PackageIdentity, dep npmDeclaredDependency) {
	nodeID := "npm:unresolved:node_modules/" + pkg.Name
	if _, exists := b.nodeIDs[nodeID]; exists {
		return
	}
	if !b.appendNode(Node{
		DependencyNode: model.DependencyNode{
			ID:             nodeID,
			Package:        pkg,
			Version:        nil,
			Relationship:   model.DependencyDirect,
			DependencyPath: []string{rootNodeID, nodeID},
			Dependencies:   []string{},
		},
		Constraint:    dep.constraint,
		Scopes:        dep.scopes,
		ScopeEvidence: npmManifestEvidence(dep.fields),
	}) {
		return
	}
	b.unresolved++
}

func (b *yarnGraphBuilder) appendUnresolvedTransitive(item yarnQueueItem, nodeID string, pkg model.PackageIdentity, constraint string) bool {
	if _, exists := b.nodeIDs[nodeID]; exists {
		b.addEdge(item.parentID, nodeID)
		return false
	}
	node := Node{
		DependencyNode: model.DependencyNode{
			ID:             nodeID,
			Package:        pkg,
			Version:        nil,
			Relationship:   model.DependencyTransitive,
			DependencyPath: append(append([]string(nil), item.path...), nodeID),
			Dependencies:   []string{},
		},
		Constraint:    constraint,
		Scopes:        []DependencyScope{DependencyScopeUnknown},
		ScopeEvidence: []DependencyEvidence{{Path: b.lockfilePath}},
	}
	if !b.appendNode(node) {
		return false
	}
	b.unresolved++
	b.addEdge(item.parentID, nodeID)
	return true
}

func (b *yarnGraphBuilder) addEdge(parentID, childID string) {
	index, exists := b.nodeIndex[parentID]
	if !exists {
		return
	}
	b.nodes[index].Dependencies = append(b.nodes[index].Dependencies, childID)
}
