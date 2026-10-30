package cli

import "testing"

func TestBuildAsrConfigFunASR(t *testing.T) {
	t.Parallel()
	withBin := buildAsrConfig("funasr", "", "", "", "", "/opt/yoooclaw-asr")
	funasr, _ := withBin["funasr"].(map[string]any)
	if withBin["mode"] != "funasr" || funasr["bin"] != "/opt/yoooclaw-asr" {
		t.Fatalf("with bin = %#v", withBin)
	}
	if _, ok := withBin["api"]; ok {
		t.Fatal("funasr config must not embed cloud api settings")
	}
	omit := buildAsrConfig("funasr", "ock-secret", "https://cloud.example/submit", "", "", "  ")
	if _, ok := omit["funasr"]; ok {
		t.Fatalf("empty bin should omit funasr block so env/default can apply: %#v", omit)
	}
	if _, ok := omit["api"]; ok {
		t.Fatal("funasr mode must drop api key even if the flag was set")
	}
}
