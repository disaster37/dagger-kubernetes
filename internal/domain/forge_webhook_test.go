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
