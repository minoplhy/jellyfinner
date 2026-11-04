package main

import (
	"errors"
	"testing"

	"jellyfinner/internal/pipeline"
)

func TestFinishRequest(t *testing.T) {
	boom := errors.New("boom")
	if req := finishRequest("l", "", nil, boom, true); !req.Released || req.Error != "" {
		t.Errorf("a shutdown must release the job: %+v", req)
	}
	if req := finishRequest("l", "", nil, boom, false); req.Released || req.Error != "boom" {
		t.Errorf("a real failure must be reported: %+v", req)
	}
	// A job that finished as the worker stopped is still a finished job.
	if req := finishRequest("l", "", &pipeline.Manifest{JobID: "a"}, nil, true); req.Released || len(req.Manifest) == 0 {
		t.Errorf("a finished job must report its manifest: %+v", req)
	}
}
