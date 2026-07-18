package family

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"

	"github.com/jstar0/codexfold/internal/codex"
	"github.com/jstar0/codexfold/internal/contain"
)

type GraphRelation string

const (
	GraphSeed       GraphRelation = "seed"
	GraphAncestor   GraphRelation = "ancestor"
	GraphDescendant GraphRelation = "descendant"
	GraphCollateral GraphRelation = "collateral"
	GraphNone       GraphRelation = "none"
)

type Relation string

const (
	RelationIdentical        Relation = "identical-applicable-records"
	RelationLeftContained    Relation = "left-contained-in-right"
	RelationRightContained   Relation = "right-contained-in-left"
	RelationIndependentTails Relation = "shared-prefix-independent-tails"
	RelationSharedRecords    Relation = "shared-exact-records"
	RelationUnknown          Relation = "unknown"
)

type Member struct {
	ID             string        `json:"id"`
	Title          string        `json:"title"`
	CWD            string        `json:"cwd"`
	RolloutPath    string        `json:"rollout_path"`
	Archived       bool          `json:"archived"`
	GitBranch      string        `json:"git_branch,omitempty"`
	RelationToSeed GraphRelation `json:"relation_to_seed"`
}

type Report struct {
	SeedID            string            `json:"seed_id"`
	Members           []Member          `json:"members"`
	Edges             []codex.SpawnEdge `json:"edges"`
	MissingSessionIDs []string          `json:"missing_session_ids,omitempty"`
}

type Comparison struct {
	LeftID               string        `json:"left_id"`
	RightID              string        `json:"right_id"`
	LeftArchived         bool          `json:"left_archived"`
	RightArchived        bool          `json:"right_archived"`
	GraphRelation        GraphRelation `json:"graph_relation"`
	Relation             Relation      `json:"relation"`
	VerifiedExact        bool          `json:"verified_exact"`
	LeftRecords          int64         `json:"left_records"`
	RightRecords         int64         `json:"right_records"`
	SharedPrefixRecords  int64         `json:"shared_prefix_records"`
	SharedRecords        int64         `json:"shared_records"`
	LeftContainedInRight bool          `json:"left_contained_in_right"`
	RightContainedInLeft bool          `json:"right_contained_in_left"`
}

type sourceSnapshot struct {
	FileInfo os.FileInfo
}

var beforeComparisonSourceValidation = func() {}

func Build(seedID string, sessions []codex.Session, edges []codex.SpawnEdge) (Report, error) {
	byID := make(map[string]codex.Session, len(sessions))
	for _, session := range sessions {
		byID[session.ID] = session
	}
	if _, exists := byID[seedID]; !exists {
		return Report{}, fmt.Errorf("session not found: %s", seedID)
	}
	adjacent := make(map[string][]string)
	for _, edge := range edges {
		adjacent[edge.ParentID] = append(adjacent[edge.ParentID], edge.ChildID)
		adjacent[edge.ChildID] = append(adjacent[edge.ChildID], edge.ParentID)
	}
	component := map[string]struct{}{seedID: {}}
	queue := []string{seedID}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		for _, next := range adjacent[current] {
			if _, seen := component[next]; seen {
				continue
			}
			component[next] = struct{}{}
			queue = append(queue, next)
		}
	}
	report := Report{SeedID: seedID}
	for sessionID := range component {
		session, exists := byID[sessionID]
		if !exists {
			report.MissingSessionIDs = append(report.MissingSessionIDs, sessionID)
			continue
		}
		report.Members = append(report.Members, Member{
			ID: session.ID, Title: session.Title, CWD: session.CWD, RolloutPath: session.RolloutPath,
			Archived: session.Archived, GitBranch: session.GitBranch,
			RelationToSeed: graphRelation(session.ID, seedID, edges),
		})
	}
	for _, edge := range edges {
		if _, parent := component[edge.ParentID]; !parent {
			continue
		}
		if _, child := component[edge.ChildID]; child {
			report.Edges = append(report.Edges, edge)
		}
	}
	sort.Slice(report.Members, func(i, j int) bool { return report.Members[i].ID < report.Members[j].ID })
	sort.Slice(report.Edges, func(i, j int) bool {
		if report.Edges[i].ParentID != report.Edges[j].ParentID {
			return report.Edges[i].ParentID < report.Edges[j].ParentID
		}
		if report.Edges[i].ChildID != report.Edges[j].ChildID {
			return report.Edges[i].ChildID < report.Edges[j].ChildID
		}
		return report.Edges[i].Status < report.Edges[j].Status
	})
	sort.Strings(report.MissingSessionIDs)
	return report, nil
}

func Compare(ctx context.Context, left codex.Session, right codex.Session, edges []codex.SpawnEdge) (Comparison, error) {
	if left.ID == "" || right.ID == "" || left.ID == right.ID || left.RolloutPath == "" || right.RolloutPath == "" {
		return Comparison{}, errors.New("distinct sessions with rollout paths are required")
	}
	leftFile, err := os.Open(left.RolloutPath)
	if err != nil {
		return Comparison{}, fmt.Errorf("open left rollout: %w", err)
	}
	defer func() { _ = leftFile.Close() }()
	rightFile, err := os.Open(right.RolloutPath)
	if err != nil {
		return Comparison{}, fmt.Errorf("open right rollout: %w", err)
	}
	defer func() { _ = rightFile.Close() }()
	leftSnapshot, err := snapshotFile(leftFile)
	if err != nil {
		return Comparison{}, fmt.Errorf("stat left rollout: %w", err)
	}
	rightSnapshot, err := snapshotFile(rightFile)
	if err != nil {
		return Comparison{}, fmt.Errorf("stat right rollout: %w", err)
	}
	leftRecords, err := scanFile(ctx, leftFile)
	if err != nil {
		return Comparison{}, fmt.Errorf("scan left rollout: %w", err)
	}
	rightRecords, err := scanFile(ctx, rightFile)
	if err != nil {
		return Comparison{}, fmt.Errorf("scan right rollout: %w", err)
	}
	if len(leftRecords) == 0 || len(rightRecords) == 0 {
		return Comparison{}, errors.New("both rollouts must contain comparable records")
	}
	result := Comparison{
		LeftID: left.ID, RightID: right.ID, LeftArchived: left.Archived, RightArchived: right.Archived,
		GraphRelation: graphRelation(left.ID, right.ID, edges), Relation: RelationUnknown,
		LeftRecords: int64(len(leftRecords)), RightRecords: int64(len(rightRecords)),
	}
	result.SharedPrefixRecords, err = sharedPrefix(ctx, leftFile, leftRecords, rightFile, rightRecords)
	if err != nil {
		return Comparison{}, err
	}
	result.SharedRecords, err = sharedRecordCount(ctx, leftFile, leftRecords, rightFile, rightRecords)
	if err != nil {
		return Comparison{}, err
	}
	leftContained, err := contain.Check(ctx,
		contain.Input{ID: left.ID, Path: left.RolloutPath},
		contain.Input{ID: right.ID, Path: right.RolloutPath},
		contain.Options{IgnoreSessionMeta: true},
	)
	if err != nil {
		return Comparison{}, err
	}
	beforeComparisonSourceValidation()
	if err := verifySourceUnchanged(left.RolloutPath, leftFile, leftSnapshot); err != nil {
		return Comparison{}, fmt.Errorf("left rollout changed during comparison: %w", err)
	}
	if err := verifySourceUnchanged(right.RolloutPath, rightFile, rightSnapshot); err != nil {
		return Comparison{}, fmt.Errorf("right rollout changed during comparison: %w", err)
	}
	rightContained, err := contain.Check(ctx,
		contain.Input{ID: right.ID, Path: right.RolloutPath},
		contain.Input{ID: left.ID, Path: left.RolloutPath},
		contain.Options{IgnoreSessionMeta: true},
	)
	if err != nil {
		return Comparison{}, err
	}
	result.LeftContainedInRight = leftContained.Contained && leftContained.VerifiedExact
	result.RightContainedInLeft = rightContained.Contained && rightContained.VerifiedExact
	switch {
	case result.LeftContainedInRight && result.RightContainedInLeft:
		result.Relation = RelationIdentical
		result.VerifiedExact = true
	case result.LeftContainedInRight:
		result.Relation = RelationLeftContained
		result.VerifiedExact = true
	case result.RightContainedInLeft:
		result.Relation = RelationRightContained
		result.VerifiedExact = true
	case result.SharedPrefixRecords > 0:
		result.Relation = RelationIndependentTails
		result.VerifiedExact = true
	case result.SharedRecords > 0:
		result.Relation = RelationSharedRecords
		result.VerifiedExact = true
	}
	return result, nil
}

func graphRelation(leftID string, rightID string, edges []codex.SpawnEdge) GraphRelation {
	if leftID == rightID {
		return GraphSeed
	}
	if reachable(leftID, rightID, edges) {
		return GraphAncestor
	}
	if reachable(rightID, leftID, edges) {
		return GraphDescendant
	}
	if connected(leftID, rightID, edges) {
		return GraphCollateral
	}
	return GraphNone
}

func reachable(start string, target string, edges []codex.SpawnEdge) bool {
	children := make(map[string][]string)
	for _, edge := range edges {
		children[edge.ParentID] = append(children[edge.ParentID], edge.ChildID)
	}
	seen := map[string]struct{}{start: {}}
	queue := []string{start}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		for _, child := range children[current] {
			if child == target {
				return true
			}
			if _, exists := seen[child]; exists {
				continue
			}
			seen[child] = struct{}{}
			queue = append(queue, child)
		}
	}
	return false
}

func connected(start string, target string, edges []codex.SpawnEdge) bool {
	adjacent := make(map[string][]string)
	for _, edge := range edges {
		adjacent[edge.ParentID] = append(adjacent[edge.ParentID], edge.ChildID)
		adjacent[edge.ChildID] = append(adjacent[edge.ChildID], edge.ParentID)
	}
	seen := map[string]struct{}{start: {}}
	queue := []string{start}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		for _, next := range adjacent[current] {
			if next == target {
				return true
			}
			if _, exists := seen[next]; exists {
				continue
			}
			seen[next] = struct{}{}
			queue = append(queue, next)
		}
	}
	return false
}

type record struct {
	digest [sha256.Size]byte
	size   int64
	start  int64
	end    int64
}

func scanFile(ctx context.Context, file *os.File) ([]record, error) {
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	reader := bufio.NewReaderSize(file, 1024*1024)
	records := make([]record, 0, 1024)
	var offset int64
	var physicalIndex int64
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		hasher := sha256.New()
		start := offset
		var size int64
		var firstCapture []byte
		captureComplete := true
		hasData := false
		reachedEOF := false
		for {
			fragment, readErr := reader.ReadSlice('\n')
			if len(fragment) > 0 {
				hasData = true
				_, _ = hasher.Write(fragment)
				size += int64(len(fragment))
				offset += int64(len(fragment))
				if physicalIndex == 0 && captureComplete {
					if len(firstCapture)+len(fragment) <= 8*1024*1024 {
						firstCapture = append(firstCapture, fragment...)
					} else {
						firstCapture = nil
						captureComplete = false
					}
				}
			}
			switch {
			case readErr == nil:
				goto complete
			case errors.Is(readErr, bufio.ErrBufferFull):
				continue
			case errors.Is(readErr, io.EOF):
				reachedEOF = true
				goto complete
			default:
				return nil, readErr
			}
		}
	complete:
		if !hasData {
			return records, nil
		}
		skip := physicalIndex == 0 && captureComplete && isSessionMeta(firstCapture)
		if !skip {
			var digest [sha256.Size]byte
			copy(digest[:], hasher.Sum(nil))
			records = append(records, record{digest: digest, size: size, start: start, end: offset})
		}
		physicalIndex++
		if reachedEOF {
			return records, nil
		}
	}
}

func isSessionMeta(data []byte) bool {
	var envelope struct {
		Type string `json:"type"`
	}
	return json.Unmarshal(bytes.TrimSuffix(data, []byte{'\n'}), &envelope) == nil && envelope.Type == "session_meta"
}

func sharedPrefix(ctx context.Context, leftFile *os.File, left []record, rightFile *os.File, right []record) (int64, error) {
	limit := min(len(left), len(right))
	var count int64
	for index := 0; index < limit; index++ {
		if left[index].size != right[index].size || left[index].digest != right[index].digest {
			break
		}
		exact, err := equalRanges(ctx, leftFile, left[index], rightFile, right[index])
		if err != nil {
			return 0, err
		}
		if !exact {
			break
		}
		count++
	}
	return count, nil
}

func sharedRecordCount(ctx context.Context, leftFile *os.File, left []record, rightFile *os.File, right []record) (int64, error) {
	type key struct {
		digest [sha256.Size]byte
		size   int64
	}
	candidates := make(map[key][]int)
	for index, item := range right {
		candidates[key{digest: item.digest, size: item.size}] = append(candidates[key{digest: item.digest, size: item.size}], index)
	}
	used := make([]bool, len(right))
	next := make(map[key]int, len(candidates))
	collisions := make(map[key][]int)
	var shared int64
	for _, leftRecord := range left {
		fingerprint := key{digest: leftRecord.digest, size: leftRecord.size}
		matched := false
		for _, index := range collisions[fingerprint] {
			if used[index] {
				continue
			}
			exact, err := equalRanges(ctx, leftFile, leftRecord, rightFile, right[index])
			if err != nil {
				return 0, err
			}
			if exact {
				used[index] = true
				shared++
				matched = true
				break
			}
		}
		if matched {
			continue
		}
		positions := candidates[fingerprint]
		for next[fingerprint] < len(positions) {
			index := positions[next[fingerprint]]
			next[fingerprint]++
			exact, err := equalRanges(ctx, leftFile, leftRecord, rightFile, right[index])
			if err != nil {
				return 0, err
			}
			if exact {
				used[index] = true
				shared++
				matched = true
				break
			}
			collisions[fingerprint] = append(collisions[fingerprint], index)
		}
	}
	return shared, nil
}

func equalRanges(ctx context.Context, leftFile *os.File, left record, rightFile *os.File, right record) (bool, error) {
	if left.size != right.size {
		return false, nil
	}
	leftReader := io.NewSectionReader(leftFile, left.start, left.size)
	rightReader := io.NewSectionReader(rightFile, right.start, right.size)
	leftBuffer := make([]byte, 128*1024)
	rightBuffer := make([]byte, len(leftBuffer))
	for remaining := left.size; remaining > 0; {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		chunk := int64(len(leftBuffer))
		if remaining < chunk {
			chunk = remaining
		}
		if _, err := io.ReadFull(leftReader, leftBuffer[:chunk]); err != nil {
			return false, err
		}
		if _, err := io.ReadFull(rightReader, rightBuffer[:chunk]); err != nil {
			return false, err
		}
		if !bytes.Equal(leftBuffer[:chunk], rightBuffer[:chunk]) {
			return false, nil
		}
		remaining -= chunk
	}
	return true, nil
}

func snapshotFile(file *os.File) (sourceSnapshot, error) {
	info, err := file.Stat()
	if err != nil {
		return sourceSnapshot{}, err
	}
	if !info.Mode().IsRegular() {
		return sourceSnapshot{}, errors.New("rollout path is not a regular file")
	}
	return sourceSnapshot{FileInfo: info}, nil
}

func verifySourceUnchanged(path string, file *os.File, before sourceSnapshot) error {
	afterHandle, err := file.Stat()
	if err != nil {
		return err
	}
	afterPath, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !os.SameFile(before.FileInfo, afterHandle) || !os.SameFile(before.FileInfo, afterPath) {
		return errors.New("rollout identity changed")
	}
	if before.FileInfo.Size() != afterHandle.Size() || !before.FileInfo.ModTime().Equal(afterHandle.ModTime()) {
		return errors.New("rollout size or modification time changed")
	}
	if afterHandle.Size() != afterPath.Size() || !afterHandle.ModTime().Equal(afterPath.ModTime()) {
		return errors.New("rollout path state differs from open file")
	}
	return nil
}
