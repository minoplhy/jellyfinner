package web

import (
	"net/http"
	"strings"
	"testing"

	"jellyfinner/internal/model"
	"jellyfinner/internal/pipeline"
)

func TestLibraryShowsEncodedSize(t *testing.T) {
	f := newFixture(t)
	f.add("done.mkv")
	f.add("waiting.mkv")
	if err := f.store.SetEncoded("done.mkv", 1, 0, "p", "sha", 412e6); err != nil {
		t.Fatal(err)
	}
	c := f.admin()
	code, body := f.get(c, "/library")
	if code != http.StatusOK {
		t.Fatalf("GET /library = %d", code)
	}
	if !strings.Contains(body, `<th class="num">Encoded</th>`) {
		t.Error("no Encoded column")
	}
	if !strings.Contains(body, "412 MB") {
		t.Error("the published title's encoded size is missing")
	}
	if !strings.Contains(body, `<td class="num">—</td>`) {
		t.Error("the unpublished title should show a dash")
	}
}

func TestJobPageShowsOutputSize(t *testing.T) {
	f := newFixture(t)
	ok, err := f.store.Enqueue(&model.Job{ID: "ep1", Path: "show/ep1.mkv", OutputPath: "show/ep1.mp4", Priority: model.PriorityNew, SourceSize: 2e9})
	if err != nil || !ok {
		t.Fatal(err)
	}
	j, err := f.store.Claim("worker1", 3600e9)
	if err != nil || j == nil {
		t.Fatal(err)
	}
	// The sidecar comes first, so the video must be found by name.
	manifest := `{"backend":"rkmpp","files":[{"name":"ep1.en.srt","size":1000},{"name":"ep1.mp4","size":500000000}]}`
	if err := f.store.Complete(j.ID, j.LeaseID, manifest, ""); err != nil {
		t.Fatal(err)
	}
	code, body := f.get(f.admin(), "/jobs/ep1")
	if code != http.StatusOK {
		t.Fatalf("GET /jobs/ep1 = %d", code)
	}
	for _, want := range []string{"<dt>Output size</dt>", "500 MB", "(25% of the source)"} {
		if !strings.Contains(body, want) {
			t.Errorf("job page lacks %q", want)
		}
	}
}

func TestVideoSize(t *testing.T) {
	files := []pipeline.File{{Name: "a.en.srt", Size: 10}, {Name: "a.mp4", Size: 7}, {Name: "a.ass", Size: 20}}
	if got := videoSize(files, "dir/a.mp4"); got != 7 {
		t.Errorf("by name = %d, want 7", got)
	}
	if got := videoSize(files, "dir/renamed.mp4"); got != 20 {
		t.Errorf("fallback = %d, want the largest, 20", got)
	}
	if got := videoSize(nil, "a.mp4"); got != 0 {
		t.Errorf("no files = %d, want 0", got)
	}
}

func TestShareOf(t *testing.T) {
	if got := shareOf(25, 100); got != "25%" {
		t.Errorf("shareOf(25, 100) = %q", got)
	}
	if got := shareOf(5, 0); got != "" {
		t.Errorf("unknown whole = %q, want empty", got)
	}
}
