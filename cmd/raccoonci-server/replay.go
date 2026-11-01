package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"raccoonci/internal/server/config"
	"raccoonci/internal/server/intake"
)

// replay sends a recorded delivery to a server, signed with the webhook
// secret (RACCOONCI_WEBHOOK_SECRET or _FILE), as GitHub would. It is how a
// delivery is tried again without pushing anything.
func replay(args []string) error {
	fs := flag.NewFlagSet("replay", flag.ExitOnError)
	url := fs.String("url", "http://127.0.0.1:18080/webhooks/github", "the server's webhook address")
	event := fs.String("event", "push", "the X-GitHub-Event of the delivery")
	delivery := fs.String("delivery", "", "the X-GitHub-Delivery id (default: a new random one; repeat one to try a redelivery)")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: raccoonci-server replay [-url U] [-event push] [-delivery ID] payload.json")
		fs.PrintDefaults()
	}
	fs.Parse(args)
	if fs.NArg() != 1 {
		fs.Usage()
		return errors.New("give the payload file")
	}
	secret, err := config.Required("RACCOONCI_WEBHOOK_SECRET")
	if err != nil {
		return err
	}
	body, err := os.ReadFile(fs.Arg(0))
	if err != nil {
		return err
	}
	id := *delivery
	if id == "" {
		b := make([]byte, 16)
		rand.Read(b)
		id = hex.EncodeToString(b)
	}
	req, err := http.NewRequest("POST", *url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Event", *event)
	req.Header.Set("X-GitHub-Delivery", id)
	req.Header.Set("X-Hub-Signature-256", intake.Sign([]byte(secret), body))
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	fmt.Printf("delivery %s: %s %s", id, resp.Status, out)
	if len(out) == 0 || out[len(out)-1] != '\n' {
		fmt.Println()
	}
	return nil
}
