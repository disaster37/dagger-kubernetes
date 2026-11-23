package domain

import "testing"

func TestParseGitHubWebhookFork(t *testing.T) {
	body := []byte(`{"ref":"refs/heads/main","after":"abc","repository":{"full_name":"acme/api"},"pull_request":{"number":7,"head":{"sha":"def","repo":{"id":1}},"base":{"repo":{"id":2}}}}`)
	ev, err := ParseForgeWebhook(ForgeGitHub, "pull_request", body)
	if err != nil {
		t.Fatal(err)
	}
	if ev.PRNumber != 7 || ev.SHA != "def" || !ev.IsFork {
		t.Fatalf("event = %+v", ev)
	}
}

func TestParseGitLabTag(t *testing.T) {
	body := []byte(`{"object_kind":"tag_push","ref":"refs/tags/v1.0.0","after":"abc","project":{"path_with_namespace":"acme/api"}}`)
	ev, err := ParseForgeWebhook(ForgeGitLab, "Tag Push Hook", body)
	if err != nil {
		t.Fatal(err)
	}
	if ev.Event != "tag" || ev.Tag != "v1.0.0" {
		t.Fatalf("event = %+v", ev)
	}
}

func TestClassifyAndParseForgeWebhook(t *testing.T) {
	cases := []struct {
		name       string
		provider   ForgeKind
		rawEvent   string
		body       string
		wantAccept bool
		wantReason string
		wantEvent  string
		wantRef    string
		wantTag    string
	}{
		{
			name: "github push branch", provider: ForgeGitHub, rawEvent: "push",
			body:       `{"ref":"refs/heads/main","after":"abc","repository":{"full_name":"acme/api"}}`,
			wantAccept: true, wantEvent: "push", wantRef: "refs/heads/main",
		},
		{
			name: "github push tag", provider: ForgeGitHub, rawEvent: "push",
			body:       `{"ref":"refs/tags/v1.0.0","after":"abc","repository":{"full_name":"acme/api"}}`,
			wantAccept: true, wantEvent: "tag", wantTag: "v1.0.0",
		},
		{
			name: "github push deleted ref", provider: ForgeGitHub, rawEvent: "push",
			body:       `{"ref":"refs/heads/gone","after":"0000000000000000000000000000000000000000","repository":{"full_name":"acme/api"}}`,
			wantReason: "ref_deleted",
		},
		{
			name: "github push empty after", provider: ForgeGitHub, rawEvent: "push",
			body:       `{"ref":"refs/heads/gone","after":"","repository":{"full_name":"acme/api"}}`,
			wantReason: "ref_deleted",
		},
		{
			name: "github pr opened", provider: ForgeGitHub, rawEvent: "pull_request",
			body:       `{"action":"opened","pull_request":{"number":1,"head":{"sha":"def","repo":{"id":1}},"base":{"repo":{"id":2}}}}`,
			wantAccept: true, wantEvent: "pull_request",
		},
		{
			name: "github pr synchronize", provider: ForgeGitHub, rawEvent: "pull_request",
			body:       `{"action":"synchronize","pull_request":{"number":1,"head":{"sha":"def","repo":{"id":1}},"base":{"repo":{"id":2}}}}`,
			wantAccept: true, wantEvent: "pull_request",
		},
		{
			name: "github pr closed", provider: ForgeGitHub, rawEvent: "pull_request",
			body:       `{"action":"closed","pull_request":{"number":1,"head":{"sha":"def","repo":{"id":1}},"base":{"repo":{"id":2}}}}`,
			wantReason: "unsupported_action",
		},
		{
			name: "github pr missing sha", provider: ForgeGitHub, rawEvent: "pull_request",
			body:       `{"action":"opened","pull_request":{"number":1,"head":{"sha":"","repo":{"id":1}},"base":{"repo":{"id":2}}}}`,
			wantReason: "unsupported_action",
		},
		{
			name: "github release published", provider: ForgeGitHub, rawEvent: "release",
			body:       `{"action":"published","release":{"tag_name":"v2.0.0"}}`,
			wantAccept: true, wantEvent: "release", wantRef: "refs/tags/v2.0.0", wantTag: "v2.0.0",
		},
		{
			name: "github release edited", provider: ForgeGitHub, rawEvent: "release",
			body:       `{"action":"edited","release":{"tag_name":"v2.0.0"}}`,
			wantReason: "unsupported_action",
		},
		{
			name: "github ping", provider: ForgeGitHub, rawEvent: "ping",
			body:       `{"zen":"hi"}`,
			wantReason: "ping",
		},
		{
			name: "github workflow_run", provider: ForgeGitHub, rawEvent: "workflow_run",
			body:       `{"action":"completed"}`,
			wantReason: "unsupported_event",
		},
		{
			name: "github status", provider: ForgeGitHub, rawEvent: "status",
			body:       `{"state":"success"}`,
			wantReason: "unsupported_event",
		},
		{
			name: "gitea push", provider: ForgeGitea, rawEvent: "push",
			body:       `{"ref":"refs/heads/main","after":"abc","repository":{"full_name":"acme/api"}}`,
			wantAccept: true, wantEvent: "push",
		},
		{
			name: "gitlab push hook", provider: ForgeGitLab, rawEvent: "Push Hook",
			body:       `{"object_kind":"push","ref":"refs/heads/main","after":"abc","project":{"path_with_namespace":"acme/api"}}`,
			wantAccept: true, wantEvent: "push",
		},
		{
			name: "gitlab tag push hook", provider: ForgeGitLab, rawEvent: "Tag Push Hook",
			body:       `{"object_kind":"tag_push","ref":"refs/tags/v1.0.0","after":"abc","project":{"path_with_namespace":"acme/api"}}`,
			wantAccept: true, wantEvent: "tag", wantTag: "v1.0.0",
		},
		{
			name: "gitlab push deleted", provider: ForgeGitLab, rawEvent: "Push Hook",
			body:       `{"object_kind":"push","ref":"refs/heads/gone","after":"0000000000000000000000000000000000000000","project":{"path_with_namespace":"acme/api"}}`,
			wantReason: "ref_deleted",
		},
		{
			name: "gitlab mr open", provider: ForgeGitLab, rawEvent: "Merge Request Hook",
			body:       `{"object_kind":"merge_request","object_attributes":{"iid":1,"action":"open","last_commit":{"id":"abc"},"source":{"id":1},"target":{"id":2}},"project":{"path_with_namespace":"acme/api"}}`,
			wantAccept: true, wantEvent: "pull_request",
		},
		{
			name: "gitlab mr merged", provider: ForgeGitLab, rawEvent: "Merge Request Hook",
			body:       `{"object_kind":"merge_request","object_attributes":{"iid":1,"action":"merge","last_commit":{"id":"abc"},"source":{"id":1},"target":{"id":2}},"project":{"path_with_namespace":"acme/api"}}`,
			wantReason: "unsupported_action",
		},
		{
			name: "gitlab release hook", provider: ForgeGitLab, rawEvent: "Release Hook",
			body:       `{"object_kind":"release","action":"create","project":{"path_with_namespace":"acme/api"},"tag":"v1.0.0"}`,
			wantAccept: true, wantEvent: "release",
		},
		{
			name: "gitlab pipeline hook", provider: ForgeGitLab, rawEvent: "Pipeline Hook",
			body:       `{"object_kind":"pipeline"}`,
			wantReason: "unsupported_event",
		},
		{
			name: "gitlab note hook", provider: ForgeGitLab, rawEvent: "Note Hook",
			body:       `{"object_kind":"note"}`,
			wantReason: "unsupported_event",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, err := ClassifyAndParseForgeWebhook(tc.provider, tc.rawEvent, []byte(tc.body))
			if err != nil {
				t.Fatal(err)
			}
			if d.RawEvent != tc.rawEvent {
				t.Fatalf("raw event = %q, want %q", d.RawEvent, tc.rawEvent)
			}
			if d.Accepted != tc.wantAccept {
				t.Fatalf("accepted = %v, want %v (reason %q)", d.Accepted, tc.wantAccept, d.Reason)
			}
			if !tc.wantAccept {
				if d.Reason != tc.wantReason {
					t.Fatalf("reason = %q, want %q", d.Reason, tc.wantReason)
				}
				if d.Event != nil {
					t.Fatalf("ignored delivery must not carry an event: %+v", d.Event)
				}
				return
			}
			if d.Event == nil {
				t.Fatal("accepted delivery must carry an event")
			}
			if tc.wantEvent != "" && d.Event.Event != tc.wantEvent {
				t.Fatalf("event = %q, want %q", d.Event.Event, tc.wantEvent)
			}
			if tc.wantRef != "" && d.Event.Ref != tc.wantRef {
				t.Fatalf("ref = %q, want %q", d.Event.Ref, tc.wantRef)
			}
			if tc.wantTag != "" && d.Event.Tag != tc.wantTag {
				t.Fatalf("tag = %q, want %q", d.Event.Tag, tc.wantTag)
			}
		})
	}
}

func TestClassifyAndParseForgeWebhookUnsupportedProvider(t *testing.T) {
	if _, err := ClassifyAndParseForgeWebhook(ForgeKind("bitbucket"), "push", []byte(`{}`)); err == nil {
		t.Fatal("unsupported provider must error")
	}
}

func TestClassifyAndParseForgeWebhookBadJSON(t *testing.T) {
	if _, err := ClassifyAndParseForgeWebhook(ForgeGitHub, "push", []byte(`{`)); err == nil {
		t.Fatal("malformed body must error")
	}
}

func TestForgeCloneURL(t *testing.T) {
	cases := []struct {
		name     string
		provider ForgeKind
		baseURL  string
		repo     string
		want     string
	}{
		{name: "github default", provider: ForgeGitHub, baseURL: "", repo: "acme/api", want: "https://github.com/acme/api.git"},
		{name: "github api url", provider: ForgeGitHub, baseURL: "https://api.github.com", repo: "acme/api", want: "https://github.com/acme/api.git"},
		{name: "ghes", provider: ForgeGitHub, baseURL: "https://ghe.example.com/api/v3", repo: "acme/api", want: "https://ghe.example.com/acme/api.git"},
		{name: "gitlab default", provider: ForgeGitLab, baseURL: "", repo: "group/proj", want: "https://gitlab.com/group/proj.git"},
		{name: "gitlab self hosted", provider: ForgeGitLab, baseURL: "https://gitlab.example.com/api/v4", repo: "group/proj", want: "https://gitlab.example.com/group/proj.git"},
		{name: "gitea", provider: ForgeGitea, baseURL: "https://gitea.example.com/api/v1", repo: "acme/api", want: "https://gitea.example.com/acme/api.git"},
		{name: "gitea no base falls back to gitea.com", provider: ForgeGitea, baseURL: "", repo: "acme/api", want: "https://gitea.com/acme/api.git"},
		{name: "already qualified", provider: ForgeGitHub, baseURL: "", repo: "https://github.com/acme/api.git", want: "https://github.com/acme/api.git"},
		{name: "already url no git", provider: ForgeGitHub, baseURL: "", repo: "https://example.com/acme/api", want: "https://example.com/acme/api"},
		{name: "repo with git suffix", provider: ForgeGitHub, baseURL: "", repo: "acme/api.git", want: "https://github.com/acme/api.git"},
		{name: "empty repo", provider: ForgeGitHub, baseURL: "", repo: "", want: ""},
		{name: "unsupported provider", provider: ForgeKind("x"), baseURL: "", repo: "acme/api", want: "acme/api"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ForgeCloneURL(tc.provider, tc.baseURL, tc.repo); got != tc.want {
				t.Fatalf("ForgeCloneURL = %q, want %q", got, tc.want)
			}
		})
	}
}
