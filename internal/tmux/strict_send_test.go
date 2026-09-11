package tmux

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

func strictTestPane(tool string) strictSnapshot {
	body := "history\n────────────\n❯ \n────────────\n? for shortcuts\n"
	y, width, height := 2, 80, 24
	if tool == "codex" {
		body = strings.Repeat("\n", 64) + "\x1b[1m›\x1b[0m \x1b[2mAsk Codex to do anything\x1b[0m \n\n  gpt-6-astra high · Context 55% used · 604K used · Fast off · Main [default]\n"
		y, width, height = 64, 141, 67
	}
	return strictSnapshot{identity: StrictPaneIdentity{SessionID: "$1", PaneID: "%2", PID: "123"}, x: 2, y: y, width: width, height: height, content: body, observedAt: time.Now()}
}

func TestStrictComposerRefusesUnknownDraftModalAndCursor(t *testing.T) {
	for _, tool := range []string{"claude", "codex"} {
		t.Run(tool, func(t *testing.T) {
			base := strictTestPane(tool)
			if got := strictEmptyComposer(tool, base); got != "" {
				t.Fatal(got)
			}
			cases := map[string]func(*strictSnapshot){
				"draft":          func(s *strictSnapshot) { s.content = strings.Replace(s.content, " \n", " partial operator draft\n", 1) },
				"wrapped_draft":  func(s *strictSnapshot) { s.content = strings.Replace(s.content, " \n", " \n  continuation\n", 1) },
				"busy":           func(s *strictSnapshot) { s.content += "esc to interrupt\n" },
				"approval":       func(s *strictSnapshot) { s.content += "Do you want to run this command?\n❯ 1. Yes\n" },
				"cursor_modal":   func(s *strictSnapshot) { s.y = 0 },
				"cursor_unknown": func(s *strictSnapshot) { s.y = 99 },
				"cursor_moved":   func(s *strictSnapshot) { s.x = 3 },
				"footer_menu":    func(s *strictSnapshot) { s.content += "Select model\n" },
				"blank":          func(s *strictSnapshot) { s.content = "" },
			}
			for name, mutate := range cases {
				t.Run(name, func(t *testing.T) {
					p := base
					mutate(&p)
					if strictEmptyComposer(tool, p) == "" {
						t.Fatal("unsafe composer accepted")
					}
				})
			}
		})
	}
}

func TestStrictLinuxClaudeComposerRefusesDraftModalAndBusyBeforeEffects(t *testing.T) {
	for _, fault := range []string{"draft", "modal", "busy"} {
		t.Run(fault, func(t *testing.T) {
			snapshot := strictTestPane("claude")
			switch fault {
			case "draft":
				snapshot.content = strings.Replace(snapshot.content, "❯ ", "❯ operator draft", 1)
			case "modal":
				snapshot.content += "Enter to confirm\n"
			case "busy":
				snapshot.content += "esc to interrupt\n"
			}
			stages, submits := 0, 0
			result, err := strictSendOnce("claude", "fixture", func(StrictPaneIdentity) error { return nil }, strictOps{
				snapshot: func(string) (strictSnapshot, error) { return snapshot, nil },
				stage:    func(string) (string, error) { stages++; return "fixture", nil },
				submit:   func(string, string) error { submits++; return nil },
			})
			if err == nil || result.Attempted || stages != 0 || submits != 0 {
				t.Fatalf("Linux %s fixture reached an effect: %+v stages=%d submits=%d", fault, result, stages, submits)
			}
		})
	}
}

func TestStrictLinuxNativeRefusalVocabularyHasNoTerminalEffect(t *testing.T) {
	// The cmd package discriminates each native fault. This transport-level
	// composition test pins the effect boundary for every fault at both verifier
	// positions: the second position may stage private server memory, but it may
	// never paste or submit terminal input.
	faults := []string{
		"missing_process", "process_churn", "wrong_start", "wrong_wall_start", "pid_reuse",
		"wrong_pgid", "wrong_sid", "background", "stopped", "zombie", "wrong_uid", "wrong_executable",
		"wrong_instance", "wrong_socket", "wrong_session", "wrong_pane", "wrong_thread", "wrong_cwd",
		"wrong_tty", "wrong_domain", "unsafe_record", "duplicate_record", "unsafe_record_path", "record_churn",
		"missing_root", "duplicate_root", "root_churn", "package_path", "package_owner", "package_mode", "package_link",
		"manifest_path", "manifest_owner", "manifest_mode", "manifest_link", "manifest_content", "manifest_churn", "package_version",
		"executable_path", "executable_owner", "executable_mode", "executable_link", "executable_content", "final_record_mutation",
		"final_record_path", "final_record_mode", "final_record_link", "final_root_mutation", "final_manifest_mutation",
		"final_executable_mutation", "final_executable_link", "final_package_mode", "final_package_content",
	}
	for _, fault := range faults {
		for _, refusalPass := range []int{1, 2} {
			t.Run(fmt.Sprintf("%s/pass_%d", fault, refusalPass), func(t *testing.T) {
				verifies, stages, drops, submits := 0, 0, 0, 0
				snapshot := strictTestPane("claude")
				result, err := strictSendOnce("claude", "fixture", func(StrictPaneIdentity) error {
					verifies++
					if verifies == refusalPass {
						return errors.New(fault)
					}
					return nil
				}, strictOps{
					snapshot: func(string) (strictSnapshot, error) { snapshot.observedAt = time.Now(); return snapshot, nil },
					stage:    func(string) (string, error) { stages++; return "private-fixture", nil },
					drop:     func(string) { drops++ },
					submit:   func(string, string) error { submits++; return nil },
				})
				wantStages := refusalPass - 1
				if err == nil || result.Attempted || result.Delivery != "refused" || submits != 0 ||
					stages != wantStages || drops != wantStages {
					t.Fatalf("native refusal crossed effect boundary: result=%+v err=%v stages=%d drops=%d submits=%d",
						result, err, stages, drops, submits)
				}
			})
		}
	}
}

func TestStrictOnceRefusalsAndAmbiguityNeverRetry(t *testing.T) {
	for _, tc := range []struct {
		name                    string
		captureFail, verifyFail int
		stageFail, submitFail   bool
		change                  string
		wantAttempt             bool
	}{
		{name: "success_is_unknown", wantAttempt: true},
		{name: "first_capture_failure", captureFail: 1},
		{name: "second_capture_failure", captureFail: 2},
		{name: "first_identity_failure", verifyFail: 1},
		{name: "thread_changes", verifyFail: 2},
		{name: "staging_failure", stageFail: true},
		{name: "paste_or_enter_failure", submitFail: true, wantAttempt: true},
		{name: "pane_changes", change: "pane"},
		{name: "pid_changes", change: "pid"},
		{name: "command_changes", change: "command"},
		{name: "session_changes", change: "session"},
		{name: "approval_appears", change: "modal"},
		{name: "draft_appears", change: "draft"},
		{name: "cursor_moves", change: "cursor"},
		{name: "snapshot_expires", change: "expired"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			captures, verifies, stages, submits, drops := 0, 0, 0, 0, 0
			ops := strictOps{
				snapshot: func(pinned string) (strictSnapshot, error) {
					captures++
					p := strictTestPane("claude")
					if captures == tc.captureFail {
						return p, errors.New("capture failed")
					}
					if captures == 2 {
						if pinned != "%2" {
							t.Fatal("second capture not pinned")
						}
						switch tc.change {
						case "pane":
							p.identity.PaneID = "%3"
						case "command":
							p.identity.Command = "shell"
						case "pid":
							p.identity.PID = "124"
						case "session":
							p.identity.SessionID = "$2"
						case "modal":
							p.content += "Enter to confirm\n"
						case "draft":
							p.content = strings.Replace(p.content, "❯ ", "❯ unfinished", 1)
						case "expired":
							p.observedAt = time.Now().Add(-time.Second)
						case "cursor":
							p.x = 0
						}
					}
					return p, nil
				},
				stage: func(body string) (string, error) {
					stages++
					if body != "hello\nworld" {
						t.Fatal("prompt altered")
					}
					if tc.stageFail {
						return "private-buffer", errors.New("stage failed")
					}
					return "private-buffer", nil
				},
				drop: func(buffer string) {
					drops++
					if buffer != "private-buffer" {
						t.Fatal(buffer)
					}
				},
				submit: func(pane, buffer string) error {
					submits++
					if pane != "%2" || buffer != "private-buffer" {
						t.Fatal("wrong target")
					}
					if tc.submitFail {
						return errors.New("ambiguous")
					}
					return nil
				},
			}
			verify := func(id StrictPaneIdentity) error {
				verifies++
				if verifies == tc.verifyFail {
					return errors.New("unverified")
				}
				return nil
			}
			result, err := strictSendOnce("claude", "hello\nworld", verify, ops)
			if result.Attempted != tc.wantAttempt {
				t.Fatalf("result %+v", result)
			}
			if tc.wantAttempt {
				if result.Delivery != "unknown" || submits != 1 {
					t.Fatalf("result=%+v sends=%d", result, submits)
				}
			} else if result.Delivery != "refused" || submits != 0 || err == nil {
				t.Fatalf("result=%+v sends=%d err=%v", result, submits, err)
			}
			if stages != drops {
				t.Fatalf("buffer leaked: stages=%d drops=%d", stages, drops)
			}
			if captures > 2 || verifies > 2 || submits > 1 || stages > 1 {
				t.Fatal("retry attempted")
			}
		})
	}
}

func TestStrictInvalidMessageHasNoEffects(t *testing.T) {
	for _, body := range []string{"", " ", "hello\rworld", "hello\x1b[201~", "hello\tworld", strings.Repeat("a", 65537), string([]byte{255})} {
		result, err := strictSendOnce("claude", body, func(StrictPaneIdentity) error { return nil }, strictOps{})
		if err == nil || result.Attempted || result.Delivery != "refused" {
			t.Fatalf("invalid input accepted: %+v", result)
		}
	}
	result, err := (&Session{VimMode: true}).StrictSendOnce("claude", "hello", nil)
	if err == nil || result.Attempted {
		t.Fatal("vim mode was mutated")
	}
}

func TestStrictProbeIdentityExecutesOnlyReadCommands(t *testing.T) {
	metadata := "$1|%2|123|2|2|0|0|1|0|0|80|24|1|claude|target|@3|/project|/dev/ttys012\n"
	commands := []string{}
	read := func(args ...string) ([]byte, error) {
		commands = append(commands, strings.Join(args, " "))
		switch args[0] {
		case "display-message":
			return []byte(metadata), nil
		case "list-clients":
			return []byte("1|1\n"), nil
		case "capture-pane":
			return []byte(strictTestPane("claude").content), nil
		default:
			return nil, fmt.Errorf("unexpected command %q", args[0])
		}
	}
	identity, err := strictProbeIdentity(func(pinned string) (strictSnapshot, error) {
		return captureStrictSnapshot("target", pinned, read, func(string) bool { return true })
	})
	if err != nil || identity.PaneID != "%2" || identity.PID != "123" {
		t.Fatalf("identity=%+v err=%v", identity, err)
	}
	want := []string{"display-message", "list-clients", "capture-pane", "display-message"}
	if len(commands) != len(want) {
		t.Fatalf("commands=%v", commands)
	}
	for index, command := range commands {
		if !strings.HasPrefix(command, want[index]+" ") {
			t.Fatalf("command %d=%q want %q", index, command, want[index])
		}
		for _, effect := range []string{"load-buffer", "paste-buffer", "send-keys", "delete-buffer", "set-buffer", "run-shell"} {
			if strings.Contains(command, effect) {
				t.Fatalf("probe invoked terminal effect %q in %q", effect, command)
			}
		}
	}
}

func TestStrictSnapshotRequiresStableLiveRawPane(t *testing.T) {
	good := "$1|%2|123|2|2|0|0|1|0|0|80|24|1|claude|target|@3|/project|/dev/ttys012\n"
	for _, tc := range []struct {
		name          string
		field         int
		value         string
		failRead      int
		drift, cooked bool
	}{
		{name: "valid", field: -1},
		{name: "dead", field: 5, value: "1"},
		{name: "copy_mode", field: 6, value: "1"},
		{name: "hidden_cursor", field: 7, value: "0"},
		{name: "input_off", field: 8, value: "1"},
		{name: "unframed_paste", field: 12, value: "0"},
		{name: "unknown_paste_mode", field: 12, value: ""},
		{name: "bad_cursor", field: 3, value: "80"},
		{name: "different_pane", field: 1, value: "%9"},
		{name: "metadata_failed", field: -1, failRead: 1},
		{name: "clients_failed", field: -1, failRead: 2},
		{name: "capture_failed", field: -1, failRead: 3},
		{name: "final_metadata_failed", field: -1, failRead: 4},
		{name: "metadata_transition", field: -1, drift: true},
		{name: "canonical_or_unknown_mode", field: -1, cooked: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			meta := good
			if tc.field >= 0 {
				f := strings.Split(strings.TrimSpace(meta), "|")
				f[tc.field] = tc.value
				meta = strings.Join(f, "|") + "\n"
			}
			read := func(args ...string) ([]byte, error) {
				calls++
				if calls == tc.failRead {
					return nil, errors.New("read failed")
				}
				if args[0] == "list-clients" {
					return nil, nil
				}
				if args[0] == "capture-pane" {
					if args[len(args)-1] != "%2" {
						t.Fatal("capture not pinned")
					}
					return []byte(strictTestPane("claude").content), nil
				}
				if calls == 4 && tc.drift {
					return []byte(strings.Replace(meta, "|2|2|", "|3|2|", 1)), nil
				}
				return []byte(meta), nil
			}
			got, err := captureStrictSnapshot("target", "%2", read, func(string) bool { return !tc.cooked })
			if tc.name == "valid" {
				if err != nil || got.identity.CWD != "/project" {
					t.Fatalf("snapshot=%+v err=%v", got, err)
				}
			} else if err == nil {
				t.Fatal("unsafe snapshot accepted")
			}
		})
	}
}

func TestStrictOperatorClientGuard(t *testing.T) {
	now := time.Unix(1000, 0)
	for _, tc := range []struct {
		clients string
		idle    bool
	}{
		{"", true}, {"1|1000\n", true}, {"0|900\n", true},
		{"0|999\n", false}, {"0|1001\n", false}, {"0|0\n", false}, {"0|unknown\n", false},
		{"unknown|0\n", false}, {"malformed\n", false}, {"1|1000\n0|999\n", false},
	} {
		if got := strictOperatorIdle(tc.clients, now); got != tc.idle {
			t.Fatalf("clients %q got %v", tc.clients, got)
		}
	}
}

// This is the actual safe lower-region capture from the enrolled Mac director.
// It contains only the source-defined main placeholder, blank row and known
// status footer. No conversation history, draft or private text is included.
func strictRecordedCodexComposer(t *testing.T) strictSnapshot {
	t.Helper()
	raw, err := os.ReadFile("testdata/codex-0.153.4-empty-composer.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Raw    string `json:"raw"`
		X      int    `json:"cursor_x"`
		Y      int    `json:"cursor_y_absolute"`
		Start  int    `json:"start_row"`
		Width  int    `json:"width"`
		Height int    `json:"height"`
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	if fixture.Start != fixture.Y || len(strings.Split(strings.TrimSuffix(fixture.Raw, "\n"), "\n"))+fixture.Start != fixture.Height {
		t.Fatal("recorded geometry inconsistent")
	}
	// The saved artifact contains only the safe lower region. Omitted history is
	// SYNTHETIC blank padding; no whole-pane/idle/live-send certificate is claimed.
	return strictSnapshot{identity: StrictPaneIdentity{SessionID: "$1", PaneID: "%2", PID: "123"}, x: fixture.X, y: fixture.Y, width: fixture.Width, height: fixture.Height, content: strings.Repeat("\n", fixture.Start) + fixture.Raw, observedAt: time.Now()}
}

func TestStrictRecordedCodexEmptyRegionAndSyntheticIdleAttempt(t *testing.T) {
	snapshot := strictRecordedCodexComposer(t)
	if got := strictEmptyComposer("codex", snapshot); got != "" {
		t.Fatalf("recorded empty composer refused: %s", got)
	}
	// Native/client/idle proof below is synthetic; the captured live runtime was not certified idle.
	submissions := 0
	result, err := strictSendOnce("codex", "synthetic prompt", func(StrictPaneIdentity) error { return nil }, strictOps{
		snapshot: func(string) (strictSnapshot, error) { snapshot.observedAt = time.Now(); return snapshot, nil },
		stage:    func(string) (string, error) { return "fixture-buffer", nil }, drop: func(string) {},
		submit: func(string, string) error { submissions++; return nil },
	})
	if err != nil || submissions != 1 || !result.Attempted || result.Delivery != "unknown" {
		t.Fatalf("guarded fixture result=%+v sends=%d err=%v", result, submissions, err)
	}
}

func TestStrictCodexPlaceholderRequiresExactStyleAndFooter(t *testing.T) {
	base := strictRecordedCodexComposer(t)
	for name, mutate := range map[string]func(*strictSnapshot){
		"typed_identical_text": func(s *strictSnapshot) { s.content = strings.Replace(s.content, "\x1b[2mAsk", "Ask", 1) },
		"plain_identical_text": func(s *strictSnapshot) { s.content = StripANSI(s.content) },
		"disabled_marker":      func(s *strictSnapshot) { s.content = strings.Replace(s.content, "\x1b[1m", "\x1b[2m", 1) },
		"missing_bold_marker":  func(s *strictSnapshot) { s.content = strings.Replace(s.content, "\x1b[1m", "", 1) },
		"color_two_not_dim": func(s *strictSnapshot) {
			s.content = strings.Replace(s.content, "\x1b[2mAsk", "\x1b[38;2;2;2;2mAsk", 1)
		},
		"unknown_style": func(s *strictSnapshot) { s.content = strings.Replace(s.content, "\x1b[2mAsk", "\x1b[2;7mAsk", 1) },
		"additional_draft": func(s *strictSnapshot) {
			s.content = strings.Replace(s.content, "to do anything", "to do anything draft", 1)
		},
		"side_composer": func(s *strictSnapshot) {
			s.content = strings.Replace(s.content, "Ask Codex to do anything", "Ask a follow-up question", 1)
		},
		"remote_image_draft": func(s *strictSnapshot) {
			lines := strings.Split(s.content, "\n")
			lines[s.y-2] = "\x1b[36m[Image #1]\x1b[0m"
			s.content = strings.Join(lines, "\n")
		},
		"cursor_moved": func(s *strictSnapshot) { s.x = 3 },
		"different_agent": func(s *strictSnapshot) {
			s.content = strings.Replace(s.content, "Main [default]", "Other [explorer]", 1)
		},
		"modal_footer_suffix": func(s *strictSnapshot) { s.content = strings.TrimSuffix(s.content, "\n") + " Enter to approve\n" },
		"unknown_shortcut_footer": func(s *strictSnapshot) {
			lines := strings.Split(s.content, "\n")
			lines[s.y+2] = "? for shortcuts · unexplained action"
			s.content = strings.Join(lines, "\n")
		},
		"fake_context_footer": func(s *strictSnapshot) {
			lines := strings.Split(s.content, "\n")
			lines[s.y+2] = "Approval menu with 50% context left"
			s.content = strings.Join(lines, "\n")
		},
	} {
		t.Run(name, func(t *testing.T) {
			snapshot := base
			mutate(&snapshot)
			if strictEmptyComposer("codex", snapshot) == "" {
				t.Fatal("draft/modal/unknown layout admitted")
			}
		})
	}
}

func TestStrictCodexModalTransitionAfterPositiveCaptureHasNoInput(t *testing.T) {
	snapshot := strictRecordedCodexComposer(t)
	captures, sends := 0, 0
	result, err := strictSendOnce("codex", "synthetic prompt", func(StrictPaneIdentity) error { return nil }, strictOps{
		snapshot: func(string) (strictSnapshot, error) {
			captures++
			if captures == 2 {
				snapshot.content = strings.Replace(snapshot.content, "Main [default]", "Choose an approval", 1)
			}
			return snapshot, nil
		},
		stage: func(string) (string, error) { return "fixture-buffer", nil }, drop: func(string) {},
		submit: func(string, string) error { sends++; return nil },
	})
	if err == nil || sends != 0 || result.Attempted {
		t.Fatalf("transition submitted: %+v sends=%d", result, sends)
	}
}

func TestStrictRecordedRegionNeverOverridesBusyNativeProof(t *testing.T) {
	snapshot := strictRecordedCodexComposer(t)
	sends := 0
	result, err := strictSendOnce("codex", "synthetic prompt", func(StrictPaneIdentity) error { return errors.New("native idle evidence unavailable") }, strictOps{
		snapshot: func(string) (strictSnapshot, error) { return snapshot, nil },
		stage:    func(string) (string, error) { t.Fatal("busy native proof must stop before staging"); return "", nil },
		submit:   func(string, string) error { sends++; return nil },
	})
	if err == nil || result.Attempted || sends != 0 {
		t.Fatalf("native proof bypassed: %+v sends=%d", result, sends)
	}
}

func TestStrictCodexBareMarkerAndSpaceDraftCannotAuthorizeAnyInput(t *testing.T) {
	for _, line := range []string{"›", "› ", "›       ", "\x1b[1m›\x1b[0m       "} {
		t.Run(line, func(t *testing.T) {
			snapshot := strictRecordedCodexComposer(t)
			lines := strings.Split(snapshot.content, "\n")
			lines[snapshot.y] = line
			snapshot.content = strings.Join(lines, "\n")
			stages, sends := 0, 0
			result, err := strictSendOnce("codex", "synthetic prompt", func(StrictPaneIdentity) error { return nil }, strictOps{
				snapshot: func(string) (strictSnapshot, error) { return snapshot, nil },
				stage:    func(string) (string, error) { stages++; return "fixture", nil }, drop: func(string) {},
				submit: func(string, string) error { sends++; return nil },
			})
			if err == nil || result.Attempted || stages != 0 || sends != 0 {
				t.Fatalf("ambiguous bare/space composer admitted: %+v stages=%d sends=%d", result, stages, sends)
			}
		})
	}
}

func TestStrictCodexRequiresMainFooterEvenWithOfficialPlaceholder(t *testing.T) {
	for _, footer := range []string{"", "? for shortcuts", "55% context left", "gpt-6-astra high · Context 55% used · 604K used · Fast off"} {
		snapshot := strictRecordedCodexComposer(t)
		lines := strings.Split(snapshot.content, "\n")
		lines[snapshot.y+2] = footer
		snapshot.content = strings.Join(lines, "\n")
		if strictEmptyComposer("codex", snapshot) == "" {
			t.Fatalf("main identity footer was bypassed with %q", footer)
		}
	}
}

// This calls the production capture boundary with renderer-derived fixtures.
// No tmux server, resize, native TUI or actual submit is involved.
func TestStrictCodexGeometryBeforeStagingThroughCapture(t *testing.T) {
	for _, fault := range []string{"measured_positive", "compact_141x4", "short_141x6", "other_width", "other_height", "missing_dimensions", "partial_capture", "extra_capture_row", "compact_inside_tall_capture", "shifted_cursor", "unknown_top_padding", "resize_during_capture"} {
		t.Run(fault, func(t *testing.T) {
			sample := strictRecordedCodexComposer(t)
			lower := strings.Join(strings.Split(sample.content, "\n")[64:], "\n")
			width, height, y := 141, 67, 64
			body := sample.content
			switch fault {
			case "compact_141x4":
				height, y, body = 4, 1, "\n"+lower
			case "short_141x6":
				height, y, body = 6, 3, strings.Repeat("\n", 3)+lower
			case "other_width":
				width = 140
			case "other_height":
				height, y, body = 68, 65, "\n"+body
			case "missing_dimensions":
				width = 0
			case "partial_capture":
				body = "\n" + lower
			case "extra_capture_row":
				body += "\n"
			case "compact_inside_tall_capture":
				y = 1
				body = "\n" + lower + strings.Repeat("\n", 63)
			case "shifted_cursor":
				y = 63
			case "unknown_top_padding":
				lines := strings.Split(body, "\n")
				lines[63] = "unproven region"
				body = strings.Join(lines, "\n")
			}
			reads, stages, drops, submits := 0, 0, 0, 0
			metadata := func(w, h, cy int) string {
				return fmt.Sprintf("$5|%%9|123|2|%d|0|0|1|0|0|%d|%d|1|node|fixture|@2|/fixture|/dev/ttys015\n", cy, w, h)
			}
			read := func(args ...string) ([]byte, error) {
				switch args[0] {
				case "display-message":
					reads++
					if fault == "resize_during_capture" && reads == 2 {
						return []byte(metadata(141, 4, 1)), nil
					}
					return []byte(metadata(width, height, y)), nil
				case "list-clients":
					return nil, nil
				case "capture-pane":
					return []byte(body), nil
				default:
					return nil, errors.New("unexpected fixture probe")
				}
			}
			result, err := strictSendOnce("codex", "synthetic prompt", func(StrictPaneIdentity) error { return nil }, strictOps{
				snapshot: func(pinned string) (strictSnapshot, error) {
					return captureStrictSnapshot("fixture", pinned, read, func(string) bool { return true })
				},
				stage: func(string) (string, error) { stages++; return "private-fixture", nil }, drop: func(string) { drops++ },
				submit: func(string, string) error { submits++; return nil },
			})
			if fault == "measured_positive" {
				if err != nil || stages != 1 || drops != 1 || submits != 1 || !result.Attempted || result.Delivery != "unknown" {
					t.Fatalf("supported synthetic capture failed: %+v err=%v stages=%d drops=%d submits=%d", result, err, stages, drops, submits)
				}
			} else if err == nil || stages != 0 || drops != 0 || submits != 0 || result.Attempted || result.Delivery != "refused" {
				t.Fatalf("unproven geometry admitted: %+v err=%v stages=%d drops=%d submits=%d", result, err, stages, drops, submits)
			}
		})
	}
}

func TestStrictCodexGeometryChangeAfterStagingNeverSubmits(t *testing.T) {
	sample := strictRecordedCodexComposer(t)
	captures, stages, drops, submits := 0, 0, 0, 0
	result, err := strictSendOnce("codex", "synthetic prompt", func(StrictPaneIdentity) error { return nil }, strictOps{
		snapshot: func(string) (strictSnapshot, error) {
			captures++
			if captures == 2 {
				sample.width = 140
			}
			sample.observedAt = time.Now()
			return sample, nil
		},
		stage: func(string) (string, error) { stages++; return "private-fixture", nil }, drop: func(string) { drops++ },
		submit: func(string, string) error { submits++; return nil },
	})
	if err == nil || result.Attempted || stages != 1 || drops != 1 || submits != 0 {
		t.Fatalf("geometry transition admitted: %+v err=%v stages=%d drops=%d submits=%d", result, err, stages, drops, submits)
	}
}
