package wecom

import (
	"bytes"
	"encoding/json"
	"log/slog"
)

// unmarshalWSFrame parses one WeCom long-connection frame.
//
// The intelligent-robot socket sometimes splices the user's text into the JSON
// string without escaping U+0000–U+001F. A device-log paste does this: its line
// breaks are raw newlines, and the ASCII column of a hex dump can contain raw
// ETX/EOT (0x03/0x04). encoding/json rejects the entire frame ("invalid
// character in string literal"), the read loop logs "invalid json" and
// continues, and the agent never starts a turn — so the bot stays silent.
// Angle-bracket tokens such as <SubHex> are ordinary text and are not the
// cause; they survive both the strict and the lenient parse.
//
// On a strict failure, raw controls that sit inside strings are rewritten to
// \u00XX and the frame is parsed again. That is the same set Python's
// json.JSONDecoder(strict=False) accepts. Quotes, backslashes, and structural
// whitespace are left alone, so already-valid commands and markdown parse
// exactly as before. The decoded Go string still contains the original
// controls, which is the text the user sent.
func unmarshalWSFrame(raw []byte) (wsFrame, error) {
	var frame wsFrame
	err := json.Unmarshal(raw, &frame)
	if err == nil {
		return frame, nil
	}
	relaxed := escapeRawJSONStringControls(raw)
	if bytes.Equal(relaxed, raw) {
		return wsFrame{}, err
	}
	var retry wsFrame
	if err2 := json.Unmarshal(relaxed, &retry); err2 != nil {
		return wsFrame{}, err
	}
	slog.Debug("wecom-ws: escaped raw control characters inside JSON strings")
	return retry, nil
}

// escapeRawJSONStringControls rewrites raw U+0000–U+001F bytes that appear
// inside JSON strings to \u00XX. Bytes outside strings, including pretty-printed
// newlines between tokens, are copied unchanged. Returns the input slice when
// nothing needs escaping.
func escapeRawJSONStringControls(raw []byte) []byte {
	inString := false
	escaped := false
	var out []byte
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		if !inString {
			if c == '"' {
				inString = true
			}
			if out != nil {
				out = append(out, c)
			}
			continue
		}
		if escaped {
			escaped = false
			if out != nil {
				out = append(out, c)
			}
			continue
		}
		switch c {
		case '\\':
			escaped = true
			if out != nil {
				out = append(out, c)
			}
			continue
		case '"':
			inString = false
			if out != nil {
				out = append(out, c)
			}
			continue
		}
		if c < 0x20 {
			if out == nil {
				out = make([]byte, 0, len(raw)+6)
				out = append(out, raw[:i]...)
			}
			out = appendJSONControlEscape(out, c)
			continue
		}
		if out != nil {
			out = append(out, c)
		}
	}
	if out == nil {
		return raw
	}
	return out
}

func appendJSONControlEscape(dst []byte, c byte) []byte {
	const hexdigits = "0123456789abcdef"
	return append(dst, '\\', 'u', '0', '0', hexdigits[c>>4], hexdigits[c&0x0f])
}
