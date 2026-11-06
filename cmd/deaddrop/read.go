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
	for _, m := range ms {
		if raw {
			m.Signature, m.SignatureNote = "", ""
			m.AttachmentSizes = nil
			for _, a := range m.Attachments {
				size := int64(-1)
				if info, err := os.Stat(filepath.Join(dir, m.Box, "files", m.ID, a)); err == nil {
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
