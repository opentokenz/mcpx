package source

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadBatchContinuationAdvancesToNextWindow(t *testing.T) {
	root := t.TempDir()
	content := strings.Join([]string{"zero", "one", "two", "three"}, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	request := BatchReadRequest{Path: "main.go", Offset: 0, Limit: 4}
	first := ReadBatch(root, []BatchReadRequest{request}, 1<<20, len("zero\n")+1, nil)
	if !first.Truncated || len(first.ContinueRequests) != 1 {
		t.Fatalf("expected continuation: %+v", first)
	}
	if first.ContinueRequests[0].Offset != 1 || first.ContinueRequests[0].LineByteOffset != 1 {
		t.Fatalf("continuation cursor=%+v, want line 1 byte 1", first.ContinueRequests[0])
	}
	second := ReadBatch(root, first.ContinueRequests, 1<<20, 1<<20, nil)
	if len(second.Results) != 1 || !second.Results[0].OK {
		t.Fatalf("continuation read failed: %+v", second)
	}
	if combined := first.Results[0].Content + second.Results[0].Content; combined != content {
		t.Fatalf("continuation lost or repeated bytes: %q want %q", combined, content)
	}
}

func TestReadBatchContinuationCompletesPartialLongLine(t *testing.T) {
	root := t.TempDir()
	const want = "abcdef\nnext\n"
	if err := os.WriteFile(filepath.Join(root, "long.txt"), []byte(want), 0o600); err != nil {
		t.Fatal(err)
	}
	requests := []BatchReadRequest{{Path: "long.txt", Offset: 0, Limit: 2}}
	var combined strings.Builder
	for page := 0; page < 10 && len(requests) > 0; page++ {
		result := ReadBatch(root, requests, 1<<20, 3, nil)
		if len(result.Results) != 1 || !result.Results[0].OK {
			t.Fatalf("page %d result=%+v", page, result)
		}
		combined.WriteString(result.Results[0].Content)
		requests = result.ContinueRequests
		if page == 0 {
			if len(requests) != 1 || requests[0].Offset != 0 || requests[0].LineByteOffset != 3 {
				t.Fatalf("first continuation must advance within line: %+v", requests)
			}
		}
	}
	if len(requests) != 0 {
		t.Fatalf("continuation did not terminate: %+v", requests)
	}
	if combined.String() != want {
		t.Fatalf("reconstructed content=%q want=%q", combined.String(), want)
	}
}

func TestReadBatchContinuesAfterPerFileTruncation(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{
		"a.txt": "a0\na1\na2\n",
		"b.txt": "b0\n",
		"c.txt": "c0\n",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	result := ReadBatch(root, []BatchReadRequest{
		{Path: "a.txt", Limit: 1},
		{Path: "b.txt", Limit: 1},
		{Path: "c.txt", Limit: 1},
	}, 1<<20, 1<<20, nil)
	if len(result.Results) != 3 {
		t.Fatalf("per-file truncation stopped batch: %+v", result)
	}
	if !result.Results[0].Truncated || result.Results[1].Content != "b0\n" || result.Results[2].Content != "c0\n" {
		t.Fatalf("unexpected batch results: %+v", result.Results)
	}
	if len(result.ContinueRequests) != 1 || result.ContinueRequests[0].Path != "a.txt" || result.ContinueRequests[0].Offset != 1 {
		t.Fatalf("missing per-file continuation: %+v", result.ContinueRequests)
	}
}

func TestReadComputesRevisionFromTheSameReadStream(t *testing.T) {
	root := t.TempDir()
	content := []byte("package demo\r\n\r\nconst Value = 1\r\n")
	if err := os.WriteFile(filepath.Join(root, "demo.go"), content, 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := Read(root, "demo.go", 0, 20, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if result.SHA256 == "" {
		t.Fatal("missing revision")
	}
	if result.Content != string(content) {
		t.Fatalf("content changed: %q", result.Content)
	}
}
