package recording

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	envFunASRBin           = "YOOOCLAW_ASR_BIN"
	defaultFunASRTimeout   = 30 * time.Minute
	maxFunASRStdoutBytes   = 8 << 20
	maxFunASRStderrBytes   = 8 << 10
	funasrAudioPlaceholder = "{audio}"
)

// AsrFunASRConfig 是 asr.mode=funasr 的本地 CLI 配置。
// Bin 为空时依次回退到 YOOOCLAW_ASR_BIN 和默认安装路径。
type AsrFunASRConfig struct {
	Bin        string   `json:"bin,omitempty"`
	Args       []string `json:"args,omitempty"`
	TimeoutSec int      `json:"timeoutSec,omitempty"`
}

// resolveFunASRBin 解析要执行的 FunASR CLI。
// 优先级：配置里的 funasr.bin → YOOOCLAW_ASR_BIN → ~/.openclaw/tools/yoooclaw-asr/bin/yoooclaw-asr。
func resolveFunASRBin(cfg *AsrConfig) string {
	if cfg != nil && cfg.FunASR != nil {
		if bin := strings.TrimSpace(cfg.FunASR.Bin); bin != "" {
			return bin
		}
	}
	if bin := strings.TrimSpace(os.Getenv(envFunASRBin)); bin != "" {
		return bin
	}
	return defaultFunASRBin()
}

func defaultFunASRBin() string {
	home, err := os.UserHomeDir()
	if err != nil || strings.TrimSpace(home) == "" {
		return filepath.Join(".openclaw", "tools", "yoooclaw-asr", "bin", "yoooclaw-asr")
	}
	return filepath.Join(home, ".openclaw", "tools", "yoooclaw-asr", "bin", "yoooclaw-asr")
}

func initializeFunASR(cfg *AsrConfig) AsrInitResult {
	bin := resolveFunASRBin(cfg)
	if err := checkFunASRBin(bin); err != nil {
		return AsrInitResult{OK: false, Mode: "funasr", Provider: "funasr", Bin: bin, Error: err.Error()}
	}
	return AsrInitResult{OK: true, Mode: "funasr", Provider: "funasr", Bin: bin, KeyConfigured: false}
}

func checkFunASRBin(bin string) error {
	bin = strings.TrimSpace(bin)
	if bin == "" {
		return errors.New("FunASR 二进制路径为空")
	}
	candidate := bin
	if !strings.ContainsRune(bin, os.PathSeparator) {
		resolved, err := exec.LookPath(bin)
		if err != nil {
			return fmt.Errorf("FunASR 二进制不存在: %s", bin)
		}
		candidate = resolved
	}
	info, err := os.Stat(candidate)
	if err != nil {
		return fmt.Errorf("FunASR 二进制不存在: %s", bin)
	}
	if info.IsDir() {
		return fmt.Errorf("FunASR 路径是目录: %s", bin)
	}
	return nil
}

func funasrTimeout(cfg *AsrFunASRConfig) time.Duration {
	if cfg != nil && cfg.TimeoutSec > 0 {
		return time.Duration(cfg.TimeoutSec) * time.Second
	}
	return defaultFunASRTimeout
}

// funasrCommandArgs 组装 argv。args 中的 {audio} 换成绝对路径；
// 没有占位符时把路径追加到末尾，保证二进制总能拿到本地音频。
func funasrCommandArgs(cfg *AsrFunASRConfig, audioPath string) []string {
	if cfg == nil || len(cfg.Args) == 0 {
		return []string{audioPath}
	}
	out := make([]string, 0, len(cfg.Args)+1)
	sawAudio := false
	for _, arg := range cfg.Args {
		if strings.Contains(arg, funasrAudioPlaceholder) {
			sawAudio = true
			arg = strings.ReplaceAll(arg, funasrAudioPlaceholder, audioPath)
		}
		out = append(out, arg)
	}
	if !sawAudio {
		out = append(out, audioPath)
	}
	return out
}

func transcribeWithFunASR(audioFilePath string, cfg *AsrFunASRConfig, logger Logger) TranscriptionResult {
	bin := resolveFunASRBin(&AsrConfig{Mode: "funasr", FunASR: cfg})
	if err := checkFunASRBin(bin); err != nil {
		return TranscriptionResult{OK: false, Error: err.Error()}
	}
	args := funasrCommandArgs(cfg, audioFilePath)
	ctx, cancel := context.WithTimeout(context.Background(), funasrTimeout(cfg))
	defer cancel()

	cmd := exec.CommandContext(ctx, bin, args...)
	var stdout, stderr cappedBuffer
	stdout.max = maxFunASRStdoutBytes
	stderr.max = maxFunASRStderrBytes
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	logger.Info("[asr-funasr] 调用本地 CLI: bin=" + bin)

	err := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		return TranscriptionResult{OK: false, Error: "FunASR 超时"}
	}
	if stdout.truncated {
		return TranscriptionResult{OK: false, Error: "FunASR 输出超过上限"}
	}
	if err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = err.Error()
		}
		return TranscriptionResult{OK: false, Error: "FunASR 执行失败: " + truncate(detail, 400)}
	}
	return mapFunASROutput(stdout.Bytes())
}

type cappedBuffer struct {
	buf       bytes.Buffer
	n         int
	max       int
	truncated bool
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	if c.max <= 0 {
		c.max = maxFunASRStdoutBytes
	}
	if c.n >= c.max {
		c.truncated = true
		return len(p), nil
	}
	remain := c.max - c.n
	if len(p) > remain {
		_, _ = c.buf.Write(p[:remain])
		c.n += remain
		c.truncated = true
		return len(p), nil
	}
	_, _ = c.buf.Write(p)
	c.n += len(p)
	return len(p), nil
}

func (c *cappedBuffer) Bytes() []byte { return c.buf.Bytes() }

func (c *cappedBuffer) String() string { return c.buf.String() }

type funasrJSON struct {
	Text       string          `json:"text"`
	Title      string          `json:"title"`
	Summary    string          `json:"summary"`
	Segments   []funasrSegment `json:"segments"`
	Transcript []funasrSegment `json:"transcript"`
}

type funasrSegment struct {
	Text      string   `json:"text"`
	Content   string   `json:"content"`
	StartMS   *float64 `json:"startMs"`
	EndMS     *float64 `json:"endMs"`
	StartTime *float64 `json:"startTime"`
	EndTime   *float64 `json:"endTime"`
	SpeakerID *int     `json:"speakerId"`
}

func mapFunASROutput(raw []byte) TranscriptionResult {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return TranscriptionResult{OK: false, Error: "FunASR 输出为空"}
	}
	if result, parsed := parseFunASRJSON(trimmed); parsed {
		return result
	}
	return TranscriptionResult{
		OK:          true,
		Text:        trimmed,
		SourceInfo:  TranscriptSource{Provider: "funasr"},
		RawResponse: trimmed,
	}
}

func parseFunASRJSON(text string) (TranscriptionResult, bool) {
	if text[0] != '{' && text[0] != '[' {
		return TranscriptionResult{}, false
	}
	if text[0] == '[' {
		var items []funasrSegment
		if json.Unmarshal([]byte(text), &items) != nil {
			return TranscriptionResult{}, false
		}
		return funasrSegmentsResult("", "", "", items, json.RawMessage(text)), true
	}
	var payload funasrJSON
	if json.Unmarshal([]byte(text), &payload) != nil {
		return TranscriptionResult{}, false
	}
	segs := payload.Segments
	if len(segs) == 0 {
		segs = payload.Transcript
	}
	return funasrSegmentsResult(payload.Text, payload.Title, payload.Summary, segs, json.RawMessage(text)), true
}

func funasrSegmentsResult(explicitText, title, summary string, segs []funasrSegment, raw any) TranscriptionResult {
	segments := make([]TranscriptSegment, 0, len(segs))
	parts := make([]string, 0, len(segs))
	for _, seg := range segs {
		content := strings.TrimSpace(firstNonEmpty(seg.Text, seg.Content))
		if content == "" {
			continue
		}
		start := seg.StartMS
		if start == nil {
			start = seg.StartTime
		}
		end := seg.EndMS
		if end == nil {
			end = seg.EndTime
		}
		segments = append(segments, TranscriptSegment{
			Text: content, StartMS: start, EndMS: end, SpeakerID: seg.SpeakerID,
		})
		parts = append(parts, content)
	}
	text := strings.TrimSpace(explicitText)
	if text == "" {
		text = strings.Join(parts, "\n\n")
	}
	if text == "" && len(segments) == 0 {
		return TranscriptionResult{OK: false, Error: "FunASR 输出为空"}
	}
	return TranscriptionResult{
		OK:          true,
		Text:        text,
		Segments:    segments,
		Summary:     strings.TrimSpace(title),
		SummaryText: strings.TrimSpace(summary),
		SourceInfo:  TranscriptSource{Provider: "funasr"},
		RawResponse: raw,
	}
}
