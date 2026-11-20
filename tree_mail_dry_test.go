package main

import (
	"bytes"
	"net/mail"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestTreeMailDry — TREE_MAIL_DRY=<dir of .eml>: what the loop would make of
// real mails, without a gateway, a peer or any state. Triage tool.
func TestTreeMailDry(t *testing.T) {
	dir := os.Getenv("TREE_MAIL_DRY")
	if dir == "" {
		t.Skip("TREE_MAIL_DRY not set")
	}
	files, _ := filepath.Glob(filepath.Join(dir, "*.eml"))
	for _, f := range files {
		raw, _ := os.ReadFile(f)
		msg, err := mail.ReadMessage(bytes.NewReader(raw))
		if err != nil {
			t.Errorf("%s: %v", f, err)
			continue
		}
		atts, more := tmAttachmentsOf(msg.Header, msg.Body)
		t.Logf("%s: subject %q, %d attachments (more=%v)", filepath.Base(f)[:8], tmHeader(msg.Header, "Subject"), len(atts), more)
		for _, a := range atts {
			rows, err := tmParseAttachment(a.name, a.data)
			if err != nil {
				t.Logf("  %s (%d B): %v", a.name, len(a.data), err)
				continue
			}
			same, ref := 0, map[string]int{}
			for _, r := range rows {
				its, why := tmDiff(r, "mdry", time.Now())
				if why != "" {
					ref[why]++
				} else if len(its) == 0 {
					same++
				}
				for _, it := range its {
					t.Logf("    row %d %q → %s (add=%v)", it.row, it.name, it.what, it.add)
				}
			}
			t.Logf("  %s (%d B): %d points, %d unchanged, refused %v", a.name, len(a.data), len(rows), same, ref)
		}
	}
}
