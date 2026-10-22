package settings

import (
	"reflect"
	"testing"
)

func TestMirrorEnvAndRoundTrip(t *testing.T) {
	if env := (Settings{}).Env(); len(env) != 0 {
		t.Errorf("default env %v", env)
	}
	s := Settings{Mirror: MirrorCN, GitHubMirror: "https://ghfast.top/"}
	want := []string{
		"KANADE_HF_ENDPOINT=https://hf-mirror.com",
		"KANADE_GITHUB_MIRROR=https://ghfast.top/",
		"UV_PYTHON_INSTALL_MIRROR=https://ghfast.top/https://github.com/astral-sh/python-build-standalone/releases/download",
	}
	if got := s.Env(); !reflect.DeepEqual(got, want) {
		t.Errorf("env %v", got)
	}
	if env := (Settings{HFToken: " hf_abc "}).Env(); !reflect.DeepEqual(env, []string{"KANADE_HF_TOKEN=hf_abc"}) {
		t.Errorf("token env %v", env)
	}
	dir := t.TempDir()
	if err := Save(dir, s); err != nil {
		t.Fatal(err)
	}
	if got, err := Load(dir); err != nil || got != s {
		t.Errorf("round trip %+v %v", got, err)
	}
}
