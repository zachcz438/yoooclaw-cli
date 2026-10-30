package recording

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeFakeFunASR(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "yoooclaw-asr")
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestFunASRInvokesConfiguredBinaryAndMapsText(t *testing.T) {
	argsFile := filepath.Join(t.TempDir(), "args.txt")
	bin := writeFakeFunASR(t, `#!/bin/sh
if [ "$#" -ne 1 ]; then
  echo "expected 1 arg, got $#" >&2
  exit 2
fi
printf '%s' "$1" > `+shellQuote(argsFile)+`
printf '%s' '{"text":"实验室转写","segments":[{"text":"实验室转写","startMs":0,"endMs":1200,"speakerId":1}]}'
`)

	dir := t.TempDir()
	storage := NewStorage(filepath.Join(dir, "recordings"), testLogger{t})
	if err := storage.Init(); err != nil {
		t.Fatal(err)
	}
	const id = "rec_funasr"
	meta := Metadata{
		Name: "实验室录音", DurationSec: 2, CreatedAt: "2026-10-30T12:00:00+08:00",
		OssAudioURL: "https://oss.invalid/rec_funasr.ogg",
	}
	if _, err := storage.Ingest(id, meta, "lab"); err != nil {
		t.Fatal(err)
	}
	audioPath := storage.AudioFilePath(id, meta.OssAudioURL)
	if err := os.WriteFile(audioPath, []byte("fake-audio"), 0o600); err != nil {
		t.Fatal(err)
	}

	var events []StatusEvent
	err := TriggerTranscription(id, storage, AsrConfig{
		Mode:   "funasr",
		FunASR: &AsrFunASRConfig{Bin: bin},
	}, testLogger{t}, SyncOptions{NotifyStatus: func(event StatusEvent) {
		events = append(events, event)
	}})
	if err != nil {
		t.Fatal(err)
	}

	gotArgs, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(gotArgs) != audioPath {
		t.Fatalf("binary args = %q, want audio path %q", gotArgs, audioPath)
	}
	if strings.Contains(string(gotArgs), "model-proxy") || strings.Contains(string(gotArgs), "oss.invalid") {
		t.Fatalf("binary received cloud URL or proxy path: %q", gotArgs)
	}

	entry, ok := storage.FindByID(id)
	if !ok {
		t.Fatal("recording missing after transcribe")
	}
	if entry.Status != StatusTranscribed {
		t.Fatalf("status = %s, want transcribed", entry.Status)
	}
	raw, err := os.ReadFile(filepath.Join(storage.TranscriptDataDir(), TranscriptDataFilename(id)))
	if err != nil {
		t.Fatal(err)
	}
	doc, parsed := ParseTranscriptDocument(raw)
	if !parsed {
		t.Fatalf("transcript document invalid: %s", raw)
	}
	if doc.Source.Provider != "funasr" || doc.Normalized.Text != "实验室转写" {
		t.Fatalf("mapped document = provider %q text %q", doc.Source.Provider, doc.Normalized.Text)
	}

	if len(events) < 2 {
		t.Fatalf("want transcribing + transcribed events, got %#v", events)
	}
	if events[0].TransferStatus != StatusTranscribing {
		t.Fatalf("first status = %s", events[0].TransferStatus)
	}
	final := events[len(events)-1]
	if final.TransferStatus != StatusTranscribed {
		t.Fatalf("final status = %s", final.TransferStatus)
	}
	if len(final.Transcript) != 1 || final.Transcript[0].Content != "实验室转写" {
		t.Fatalf("status transcript = %#v", final.Transcript)
	}
	if final.Transcript[0].SpeakerID == nil || *final.Transcript[0].SpeakerID != 1 {
		t.Fatalf("speaker = %#v", final.Transcript[0].SpeakerID)
	}
	if final.Transcript[0].StartTime == nil || *final.Transcript[0].StartTime != 0 ||
		final.Transcript[0].EndTime == nil || *final.Transcript[0].EndTime != 1200 {
		t.Fatalf("times = %#v %#v", final.Transcript[0].StartTime, final.Transcript[0].EndTime)
	}
}

func TestFunASRUsesEnvBinWhenConfigOmitsPath(t *testing.T) {
	bin := writeFakeFunASR(t, "#!/bin/sh\nprintf '%s' '来自环境变量'\n")
	t.Setenv(envFunASRBin, bin)
	audio := filepath.Join(t.TempDir(), "clip.ogg")
	if err := os.WriteFile(audio, []byte("a"), 0o600); err != nil {
		t.Fatal(err)
	}
	result := TranscribeAudio(audio, AsrConfig{Mode: "funasr"}, testLogger{t}, "https://oss.invalid/should-not-be-sent", 1000)
	if !result.OK || result.Text != "来自环境变量" || result.SourceInfo.Provider != "funasr" {
		t.Fatalf("env binary result = %+v", result)
	}
}

func TestFunASRConfigBinOverridesEnv(t *testing.T) {
	envBin := writeFakeFunASR(t, "#!/bin/sh\nprintf '%s' 'env'\n")
	cfgBin := writeFakeFunASR(t, "#!/bin/sh\nprintf '%s' 'config'\n")
	t.Setenv(envFunASRBin, envBin)
	audio := filepath.Join(t.TempDir(), "clip.ogg")
	if err := os.WriteFile(audio, []byte("a"), 0o600); err != nil {
		t.Fatal(err)
	}
	result := TranscribeAudio(audio, AsrConfig{Mode: "funasr", FunASR: &AsrFunASRConfig{Bin: cfgBin}}, testLogger{t}, "", 0)
	if !result.OK || result.Text != "config" {
		t.Fatalf("config bin should win, got %+v", result)
	}
}

func TestFunASREmptyOutputDoesNotSucceed(t *testing.T) {
	bin := writeFakeFunASR(t, "#!/bin/sh\nprintf '%s' '   '\n")
	audio := filepath.Join(t.TempDir(), "clip.ogg")
	if err := os.WriteFile(audio, []byte("a"), 0o600); err != nil {
		t.Fatal(err)
	}
	result := TranscribeAudio(audio, AsrConfig{Mode: "funasr", FunASR: &AsrFunASRConfig{Bin: bin}}, testLogger{t}, "", 0)
	if result.OK || !strings.Contains(result.Error, "输出为空") {
		t.Fatalf("empty output = %+v", result)
	}
}

func TestFunASRCommandFailure(t *testing.T) {
	bin := writeFakeFunASR(t, "#!/bin/sh\necho '模型没起来' >&2\nexit 3\n")
	audio := filepath.Join(t.TempDir(), "clip.ogg")
	if err := os.WriteFile(audio, []byte("a"), 0o600); err != nil {
		t.Fatal(err)
	}
	result := TranscribeAudio(audio, AsrConfig{Mode: "funasr", FunASR: &AsrFunASRConfig{Bin: bin}}, testLogger{t}, "", 0)
	if result.OK || !strings.Contains(result.Error, "模型没起来") {
		t.Fatalf("failure result = %+v", result)
	}
}

func TestFunASRTimeout(t *testing.T) {
	bin := writeFakeFunASR(t, "#!/bin/sh\nexec sleep 30\n")
	audio := filepath.Join(t.TempDir(), "clip.ogg")
	if err := os.WriteFile(audio, []byte("a"), 0o600); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	result := TranscribeAudio(audio, AsrConfig{
		Mode: "funasr", FunASR: &AsrFunASRConfig{Bin: bin, TimeoutSec: 1},
	}, testLogger{t}, "", 0)
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("timeout took %s", elapsed)
	}
	if result.OK || !strings.Contains(result.Error, "超时") {
		t.Fatalf("timeout result = %+v", result)
	}
}

func TestFunASRArgsPlaceholder(t *testing.T) {
	argsFile := filepath.Join(t.TempDir(), "args.txt")
	bin := writeFakeFunASR(t, `#!/bin/sh
printf '%s\n' "$@" > `+shellQuote(argsFile)+`
printf '%s' '{"transcript":[{"content":"分段文本","startTime":5,"endTime":9}]}'
`)
	audio := filepath.Join(t.TempDir(), "clip.wav")
	if err := os.WriteFile(audio, []byte("a"), 0o600); err != nil {
		t.Fatal(err)
	}
	result := TranscribeAudio(audio, AsrConfig{
		Mode: "funasr",
		FunASR: &AsrFunASRConfig{
			Bin:  bin,
			Args: []string{"run", "--audio", "{audio}", "--apply-glossary"},
		},
	}, testLogger{t}, "", 0)
	if !result.OK || result.Text != "分段文本" || len(result.Segments) != 1 {
		t.Fatalf("placeholder result = %+v", result)
	}
	if result.Segments[0].StartMS == nil || *result.Segments[0].StartMS != 5 ||
		result.Segments[0].EndMS == nil || *result.Segments[0].EndMS != 9 {
		t.Fatalf("segment times = %#v", result.Segments)
	}
	got, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	want := "run\n--audio\n" + audio + "\n--apply-glossary\n"
	if string(got) != want {
		t.Fatalf("args = %q, want %q", got, want)
	}
}

func TestResolveFunASRBinDefault(t *testing.T) {
	t.Setenv(envFunASRBin, "")
	got := resolveFunASRBin(&AsrConfig{Mode: "funasr"})
	suffix := filepath.Join(".openclaw", "tools", "yoooclaw-asr", "bin", "yoooclaw-asr")
	if !strings.HasSuffix(got, suffix) {
		t.Fatalf("default bin = %q, want suffix %q", got, suffix)
	}
	if ValidateAsrConfig(&AsrConfig{Mode: "funasr"}) != "" {
		t.Fatal("funasr mode should validate without a cloud key")
	}
}

func TestInitializeFunASRDoesNotRequireAPIKey(t *testing.T) {
	missing := InitializeAsr(&AsrConfig{Mode: "funasr", FunASR: &AsrFunASRConfig{Bin: filepath.Join(t.TempDir(), "missing")}})
	if missing.OK || missing.KeyConfigured || strings.Contains(missing.Error, "API Key") {
		t.Fatalf("missing binary = %+v", missing)
	}
	bin := writeFakeFunASR(t, "#!/bin/sh\nexit 0\n")
	ok := InitializeAsr(&AsrConfig{Mode: "funasr", FunASR: &AsrFunASRConfig{Bin: bin}})
	if !ok.OK || ok.KeyConfigured || ok.Provider != "funasr" || ok.Bin != bin {
		t.Fatalf("init = %+v", ok)
	}
}

func TestLoadFunASRConfig(t *testing.T) {
	dir := t.TempDir()
	raw := []byte(`{"mode":"funasr","funasr":{"bin":"/opt/yoooclaw-asr","args":["--audio","{audio}"],"timeoutSec":15}}`)
	if err := os.WriteFile(filepath.Join(dir, "asr-config.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := LoadLocalAsrConfig(dir)
	if cfg == nil || cfg.Mode != "funasr" || cfg.FunASR == nil || cfg.FunASR.Bin != "/opt/yoooclaw-asr" || cfg.FunASR.TimeoutSec != 15 {
		t.Fatalf("loaded = %+v", cfg)
	}
	if len(cfg.FunASR.Args) != 2 || cfg.FunASR.Args[1] != "{audio}" {
		t.Fatalf("args = %#v", cfg.FunASR.Args)
	}
	blob, err := json.Marshal(cfg.FunASR)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(blob), `"bin":"/opt/yoooclaw-asr"`) {
		t.Fatalf("remarshal = %s", blob)
	}
}

func shellQuote(path string) string {
	return "'" + strings.ReplaceAll(path, "'", `'\''`) + "'"
}
