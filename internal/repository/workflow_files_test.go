package repository

import "testing"

func TestWorkflowFileParser(t *testing.T) {
	data := []byte(`
name: release
on:
  push: true
  schedule: ["0 2 * * *"]
  workflow_dispatch:
    inputs:
      version:
        type: string
        required: true
jobs:
  build:
    call: build
    timeout: 2d
    artifacts:
      - name: bin
        path: ./out
  deploy:
    needs: [build]
    environment: prod
    approval_timeout: 24h
    trigger:
      repo: acme/infra
      workflow: deploy
      wait: true
      timeout: 1h
`)
	wf, err := NewWorkflowFileParser().Parse("release.yaml", data)
	if err != nil {
		t.Fatal(err)
	}
	if !wf.On.Push || len(wf.On.Schedule) != 1 || wf.On.WorkflowDispatch == nil {
		t.Fatalf("triggers = %+v", wf.On)
	}
	if wf.Jobs["build"].Timeout.Hours() != 48 {
		t.Fatalf("timeout = %v", wf.Jobs["build"].Timeout)
	}
	if wf.Jobs["deploy"].Trigger == nil || !wf.Jobs["deploy"].Trigger.Wait {
		t.Fatalf("trigger = %+v", wf.Jobs["deploy"].Trigger)
	}
	if wf.Jobs["deploy"].ApprovalTimeout.Hours() != 24 {
		t.Fatalf("approval timeout = %v", wf.Jobs["deploy"].ApprovalTimeout)
	}
}
