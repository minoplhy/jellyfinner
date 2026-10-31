package job

import "testing"

func valid() Job {
	return Job{ID: "abc", Source: "origin:/a.mkv", Incoming: "origin:/incoming", OutputPath: "Movies/A/A.mp4"}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Job)
		wantErr bool
	}{
		{"valid", func(*Job) {}, false},
		{"missing id", func(j *Job) { j.ID = "" }, true},
		{"id with slash", func(j *Job) { j.ID = "../x" }, true},
		{"missing source", func(j *Job) { j.Source = "" }, true},
		{"missing incoming", func(j *Job) { j.Incoming = "" }, true},
		{"negative revision", func(j *Job) { j.Revision = -1 }, true},
		{"absolute output", func(j *Job) { j.OutputPath = "/etc/x.mp4" }, true},
		{"traversal", func(j *Job) { j.OutputPath = "../x.mp4" }, true},
		{"inner traversal", func(j *Job) { j.OutputPath = "a/../../x.mp4" }, true},
		{"not mp4", func(j *Job) { j.OutputPath = "a/x.mkv" }, true},
		{"backslash", func(j *Job) { j.OutputPath = `a\x.mp4` }, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			j := valid()
			tt.mutate(&j)
			if err := j.Validate(); (err != nil) != tt.wantErr {
				t.Errorf("Validate() = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestComputeID(t *testing.T) {
	base := ComputeID("a", 1, 2, "p", 0)
	if base != ComputeID("a", 1, 2, "p", 0) {
		t.Error("same input must give the same ID")
	}
	for name, id := range map[string]string{
		"path":     ComputeID("b", 1, 2, "p", 0),
		"size":     ComputeID("a", 9, 2, "p", 0),
		"mtime":    ComputeID("a", 1, 9, "p", 0),
		"profile":  ComputeID("a", 1, 2, "q", 0),
		"revision": ComputeID("a", 1, 2, "p", 1),
	} {
		if id == base {
			t.Errorf("a different %s must give a different ID", name)
		}
	}
}
