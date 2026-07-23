package family

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/samekind/codexfold/internal/codex"
)

func TestBuildReportsConnectedFamilyStateWithoutInferringUsefulness(t *testing.T) {
	sessions := []codex.Session{
		{ID: "root", Title: "Root", RolloutPath: "/rollouts/root.jsonl", Archived: false},
		{ID: "child", Title: "Child", RolloutPath: "/rollouts/child.jsonl", Archived: true},
		{ID: "grandchild", Title: "Grandchild", RolloutPath: "/rollouts/grandchild.jsonl", Archived: false},
		{ID: "unrelated", Title: "Unrelated", RolloutPath: "/rollouts/unrelated.jsonl", Archived: true},
	}
	edges := []codex.SpawnEdge{
		{ParentID: "root", ChildID: "child", Status: "closed"},
		{ParentID: "child", ChildID: "grandchild", Status: "open"},
		{ParentID: "root", ChildID: "missing", Status: "closed"},
	}
	report, err := Build("child", sessions, edges)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Members) != 3 || len(report.Edges) != 3 || len(report.MissingSessionIDs) != 1 || report.MissingSessionIDs[0] != "missing" {
		t.Fatalf("family report = %#v", report)
	}
	byID := make(map[string]Member)
	for _, member := range report.Members {
		byID[member.ID] = member
	}
	if byID["child"].RelationToSeed != GraphSeed || !byID["child"].Archived {
		t.Fatalf("seed member = %#v", byID["child"])
	}
	if byID["root"].RelationToSeed != GraphAncestor || byID["grandchild"].RelationToSeed != GraphDescendant {
		t.Fatalf("graph relations = %#v", byID)
	}
}

func TestCompareClassifiesExactContainmentIndependentTailsAndUnknown(t *testing.T) {
	root := t.TempDir()
	write := func(name string, records ...string) codex.Session {
		path := filepath.Join(root, name+".jsonl")
		data := ""
		for _, record := range records {
			data += record + "\n"
		}
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
		return codex.Session{ID: name, RolloutPath: path, Archived: name == "left"}
	}
	metaLeft := `{"type":"session_meta","id":"left"}`
	metaRight := `{"type":"session_meta","id":"right"}`
	a := `{"value":"a"}`
	b := `{"value":"b"}`
	c := `{"value":"c"}`
	x := `{"value":"x"}`
	y := `{"value":"y"}`

	identical, err := Compare(context.Background(),
		write("identical-left", metaLeft, a, b), write("identical-right", metaRight, a, b), nil,
	)
	if err != nil || identical.Relation != RelationIdentical || !identical.VerifiedExact {
		t.Fatalf("identical comparison = %#v err=%v", identical, err)
	}

	contained, err := Compare(context.Background(),
		write("left", metaLeft, a, b), write("container", metaRight, x, a, b, y), nil,
	)
	if err != nil || contained.Relation != RelationLeftContained || !contained.LeftContainedInRight || !contained.VerifiedExact {
		t.Fatalf("contained comparison = %#v err=%v", contained, err)
	}

	tails, err := Compare(context.Background(),
		write("tail-left", metaLeft, a, b), write("tail-right", metaRight, a, c), nil,
	)
	if err != nil || tails.Relation != RelationIndependentTails || tails.SharedPrefixRecords != 1 || tails.SharedRecords != 1 {
		t.Fatalf("independent-tail comparison = %#v err=%v", tails, err)
	}

	unknown, err := Compare(context.Background(),
		write("unknown-left", metaLeft, x), write("unknown-right", metaRight, y), nil,
	)
	if err != nil || unknown.Relation != RelationUnknown || unknown.SharedRecords != 0 {
		t.Fatalf("unknown comparison = %#v err=%v", unknown, err)
	}
}

func TestCompareReportsGraphEvidenceSeparatelyFromContent(t *testing.T) {
	root := t.TempDir()
	leftPath := filepath.Join(root, "left.jsonl")
	rightPath := filepath.Join(root, "right.jsonl")
	if err := os.WriteFile(leftPath, []byte("{\"v\":1}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rightPath, []byte("{\"v\":2}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	comparison, err := Compare(context.Background(),
		codex.Session{ID: "left", RolloutPath: leftPath},
		codex.Session{ID: "right", RolloutPath: rightPath},
		[]codex.SpawnEdge{{ParentID: "left", ChildID: "right", Status: "open"}},
	)
	if err != nil || comparison.GraphRelation != GraphAncestor || comparison.Relation != RelationUnknown {
		t.Fatalf("graph/content evidence was conflated: %#v err=%v", comparison, err)
	}
}

func TestCompareRejectsSourceMutationBeforeReturningEvidence(t *testing.T) {
	root := t.TempDir()
	leftPath := filepath.Join(root, "left.jsonl")
	rightPath := filepath.Join(root, "right.jsonl")
	data := []byte("{\"type\":\"session_meta\"}\n{\"value\":1}\n")
	if err := os.WriteFile(leftPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rightPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	previous := beforeComparisonSourceValidation
	beforeComparisonSourceValidation = func() {
		file, err := os.OpenFile(rightPath, os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.WriteString("{\"mutated\":true}\n"); err != nil {
			_ = file.Close()
			t.Fatal(err)
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { beforeComparisonSourceValidation = previous })
	_, err := Compare(context.Background(),
		codex.Session{ID: "left", RolloutPath: leftPath},
		codex.Session{ID: "right", RolloutPath: rightPath}, nil,
	)
	if err == nil || !strings.Contains(err.Error(), "changed during comparison") {
		t.Fatalf("source mutation error = %v", err)
	}
}

func TestCompareRepeatedRecordsAvoidsQuadraticFileReopens(t *testing.T) {
	root := t.TempDir()
	writeRepeated := func(name string) codex.Session {
		path := filepath.Join(root, name+".jsonl")
		var data strings.Builder
		data.WriteString("{\"type\":\"session_meta\",\"id\":\"")
		data.WriteString(name)
		data.WriteString("\"}\n")
		for range 1000 {
			data.WriteString("{\"value\":\"same repeated record\"}\n")
		}
		if err := os.WriteFile(path, []byte(data.String()), 0o600); err != nil {
			t.Fatal(err)
		}
		return codex.Session{ID: name, RolloutPath: path}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	comparison, err := Compare(ctx, writeRepeated("left"), writeRepeated("right"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if comparison.Relation != RelationIdentical || comparison.SharedRecords != 1000 {
		t.Fatalf("repeated comparison = %#v", comparison)
	}
}
