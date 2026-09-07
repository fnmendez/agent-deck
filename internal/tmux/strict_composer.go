package tmux

import (
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Codex rust-v0.153.4 chatwidget.rs defines this exact main placeholder.
// chat_composer.rs renders it DIM only when the textarea is empty, while the
// enabled marker is BOLD. A typed copy is not a placeholder. Side-composer and
// disabled-input placeholders are deliberately unsupported.
const strictCodexMainPlaceholder = "› Ask Codex to do anything"

// Measured full Mac footer, not a substring match. Main [default] is the source
// defined primary-agent label. Unknown/custom labels, menus and added text fail.
var strictCodexStatusFooter = regexp.MustCompile(`^gpt-6-astra (low|medium|high|xhigh|ultra) · Context (100|[0-9]{1,2})% used · [0-9]+(\.[0-9]+)?[KM]? used · Fast (off|on) · Main \[default\]$`)

// Remote/backtracked images can exist above an otherwise empty textarea.
var strictCodexImageRow = regexp.MustCompile(`(?m)^[ \t]*\[Image #[1-9][0-9]*\][ \t]*$`)

func strictCodexPlaceholder(raw string) bool {
	type cell struct {
		r         rune
		bold, dim bool
	}
	cells := []cell{}
	bold, dim := false, false
	for len(raw) > 0 {
		if raw[0] == '\x1b' {
			// Only SGR from capture-pane -e is accepted. Other terminal instructions
			// cannot become evidence merely because a generic ANSI stripper hides them.
			if !strings.HasPrefix(raw, "\x1b[") {
				return false
			}
			end := strings.IndexByte(raw, 'm')
			if end < 0 {
				return false
			}
			parameters := raw[2:end]
			codes := []int{}
			if parameters == "" {
				codes = append(codes, 0)
			} else {
				for _, parameter := range strings.Split(parameters, ";") {
					code, err := strconv.Atoi(parameter)
					if err != nil || code < 0 {
						return false
					}
					codes = append(codes, code)
				}
			}
			for i := 0; i < len(codes); i++ {
				code := codes[i]
				switch {
				case code == 0:
					bold, dim = false, false
				case code == 1:
					bold = true
				case code == 2:
					dim = true
				case code == 22:
					bold, dim = false, false
				case code == 38 || code == 48:
					// Color mode 2 and RGB components are NOT the DIM modifier.
					if i+1 >= len(codes) {
						return false
					}
					count := 0
					switch codes[i+1] {
					case 2:
						count = 3
					case 5:
						count = 1
					default:
						return false
					}
					if i+1+count >= len(codes) {
						return false
					}
					for j := i + 2; j <= i+1+count; j++ {
						if codes[j] > 255 {
							return false
						}
					}
					i += 1 + count
				case code == 39 || code == 49 || (code >= 30 && code <= 37) || (code >= 40 && code <= 47) || (code >= 90 && code <= 97) || (code >= 100 && code <= 107):
					// Color changes preserve text modifiers.
				default:
					return false
				}
			}
			raw = raw[end+1:]
			continue
		}
		r, size := utf8.DecodeRuneInString(raw)
		if (r == utf8.RuneError && size == 1) || r < 32 || r == 127 {
			return false
		}
		cells = append(cells, cell{r, bold, dim})
		raw = raw[size:]
	}
	for len(cells) > 0 && cells[len(cells)-1].r == ' ' {
		cells = cells[:len(cells)-1]
	}
	expected := []rune(strictCodexMainPlaceholder)
	if len(cells) != len(expected) {
		return false
	}
	for i, value := range cells {
		if value.r != expected[i] {
			return false
		}
		if i == 0 && (!value.bold || value.dim) {
			return false
		}
		if i >= 2 && (!value.dim || value.bold) {
			return false
		}
	}
	return true
}

// This release certifies only the recorded 141x67 normal-main geometry, not a
// general "large enough" screen. Preserve the actual metadata and a complete
// capture: a cropped four-row viewport cannot borrow a larger pane's metadata.
//
// In rust-v0.153.4, ChatWidget reserves the bottom pane as non-flex (one top
// inset), and BottomPane reserves Composer as non-flex before other panels.
// Composer desired height includes remote rows, separator, textarea, padding
// and footer. For this geometry, populated remote rows are allocated before a
// visible textarea; saturation removes the textarea, not just all image rows.
// See docs/strict-send.md for the source derivation and deliberately narrow scope.
func strictCodexGeometry(s strictSnapshot, lines []string) bool {
	const measuredWidth, measuredHeight, measuredCursorY = 141, 67, 64
	if s.width != measuredWidth || s.height != measuredHeight || len(lines) != s.height || s.y != measuredCursorY {
		return false
	}
	// Exact normal one-line padding and footer placement; unknown/clipped or
	// shifted layouts refuse, including an old compact viewport in a tall pane.
	return strings.TrimSpace(lines[s.y-1]) == "" && strings.TrimSpace(lines[s.y+1]) == "" &&
		s.y+2 == s.height-1 && strictCodexStatusFooter.MatchString(strings.TrimSpace(lines[s.height-1]))
}
