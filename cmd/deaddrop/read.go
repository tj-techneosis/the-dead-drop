package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Reading, and where a message's signature is checked: always on the
// reader's own machine. With the store here, that's here. With the store
// elsewhere (viaHost), the store hands the messages over as they are
// (--raw: the signature's bytes, the body), and they're checked here, with
// this machine's list of signers; the store's own list never counts.

// fetchRemote runs a reading command on the store's machine, --raw, and
// returns its messages.
func fetchRemote(cmd string, args []string) ([]*Message, error) {
	out, err := remoteBytes(viaHost, cmd, append(append([]string{}, args...), "--json", "--raw"))
	if err != nil {
		return nil, err
	}
	if cmd == "show" {
		var m Message
		if err := json.Unmarshal(out, &m); err != nil {
			return nil, fmt.Errorf("the store's answer wasn't a message: %v", err)
		}
		return []*Message{&m}, nil
	}
	var ms []*Message
	if err := json.Unmarshal(out, &ms); err != nil {
		return nil, fmt.Errorf("the store's answer wasn't a list of messages: %v", err)
	}
	return ms, nil
}

// prepare gets messages ready to print. raw: as they are, for a reader on
// another machine to check, with their attachments' sizes. Otherwise:
// checked here, and the signature's bytes left out.
func prepare(ms []*Message, raw bool, dir string) {
	if dir != "" {
		fingerprint(dir, ms)
	}
	for _, m := range ms {
		if raw {
			m.Signature, m.SignatureNote = "", ""
			m.AttachmentSizes = nil
			for _, a := range m.Attachments {
				size := int64(-1)
				if info, err := os.Stat(filepath.Join(filesDir(dir, m), a)); err == nil {
					size = info.Size()
				}
				m.AttachmentSizes = append(m.AttachmentSizes, size)
			}
			continue
		}
		verify(m)
		m.Sig = ""
	}
}

// letterOf is a short fingerprint of what every copy of a letter shares.
func letterOf(m *Message) string {
	h := sha256.Sum256([]byte(sentKey(m)))
	return hex.EncodeToString(h[:8])
}

// storeIndex maps every id in the store, in every box and pigeonhole, to its
// letter: built once per command, and only when a thread needs it.
var storeIndex map[string]string

// fingerprint sets each message's letter, and the letter its thread names,
// wherever in the store that copy is. Bodies are needed for unsigned mail.
func fingerprint(dir string, ms []*Message) {
	need := false
	for _, m := range ms {
		if m.Body != "" || m.Sig != "" {
			m.Letter = letterOf(m)
		}
		need = need || m.Thread != ""
	}
	if !need {
		return
	}
	if storeIndex == nil {
		storeIndex = map[string]string{}
		for _, box := range boxes(dir) {
			for _, x := range loadAll(dir, box, []string{"unread", "read", "archived"}, true) {
				// Ids are unique within a box; one that two boxes share
				// for different letters names neither.
				fp := letterOf(x)
				if was, ok := storeIndex[x.ID]; ok && was != fp {
					fp = ""
				}
				storeIndex[x.ID] = fp
			}
		}
	}
	for _, m := range ms {
		if m.Thread != "" {
			m.ThreadLetter = storeIndex[m.Thread]
		}
	}
}

// sigLine is the signature's state as a person reads it, under a message.
func sigLine(m *Message) string {
	switch m.Signature {
	case sigVerified:
		return "[signature: verified, " + m.SignatureNote + "]"
	case sigUnsigned:
		return "[signature: none: " + m.SignatureNote + "]"
	case sigUnknown:
		return "[signature: can't check: " + m.SignatureNote + "]"
	}
	return "[signature: BAD: " + m.SignatureNote + ". Don't trust who it says it's from.]"
}

// sigMark is the same, in one character, for a table or a menu.
func sigMark(m *Message) string {
	switch m.Signature {
	case sigVerified:
		return "✓"
	case sigUnsigned:
		return "-"
	case sigUnknown:
		return "?"
	}
	return "✗"
}

// checkAttachment holds fetched attachment bytes up against the sum the
// message records (and, if it's signed, its signature vouches for).
func checkAttachment(m *Message, name string, data []byte) error {
	for i, a := range m.Attachments {
		if a != name || i >= len(m.AttachmentSums) {
			continue
		}
		h := sha256.Sum256(data)
		if hex.EncodeToString(h[:]) != m.AttachmentSums[i] {
			return fmt.Errorf("%s in %s isn't the file that was sent: it has changed since", name, m.ID)
		}
	}
	return nil
}
